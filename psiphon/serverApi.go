/*
 * Copyright (c) 2015, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/transferstats"
)

const (
	SERVER_API_HANDSHAKE_REQUEST_NAME           = "psiphon-handshake"
	SERVER_API_CONNECTED_REQUEST_NAME           = "psiphon-connected"
	SERVER_API_STATUS_REQUEST_NAME              = "psiphon-status"
	SERVER_API_CLIENT_VERIFICATION_REQUEST_NAME = "psiphon-client-verification"
)

// ServerContext is a utility struct which holds all of the data associated
// with a Psiphon server connection. In addition to the established tunnel, this
// includes data and transport mechanisms for Psiphon API requests. Legacy servers
// offer the Psiphon API through a web service; newer servers offer the Psiphon
// API through SSH requests made directly through the tunnel's SSH client.
type ServerContext struct {
	sessionId                string
	tunnelNumber             int64
	tunnel                   *Tunnel
	psiphonHttpsClient       *http.Client
	statsRegexps             *transferstats.Regexps
	clientRegion             string
	clientUpgradeVersion     string
	serverHandshakeTimestamp string
}

// MeekStats holds extra stats that are only gathered for meek tunnels.
type MeekStats struct {
	DialAddress         string
	ResolvedIPAddress   string
	SNIServerName       string
	HostHeader          string
	TransformedHostName bool
}

// nextTunnelNumber is a monotonically increasing number assigned to each
// successive tunnel connection. The sessionId and tunnelNumber together
// form a globally unique identifier for tunnels, which is used for
// stats. Note that the number is increasing but not necessarily
// consecutive for each active tunnel in session.
var nextTunnelNumber int64

// MakeSessionId creates a new session ID. The same session ID is used across
// multi-tunnel controller runs, where each tunnel has its own ServerContext
// instance.
// In server-side stats, we now consider a "session" to be the lifetime of the
// Controller (e.g., the user's commanded start and stop) and we measure this
// duration as well as the duration of each tunnel within the session.
func MakeSessionId() (sessionId string, err error) {
	randomId, err := MakeSecureRandomBytes(PSIPHON_API_CLIENT_SESSION_ID_LENGTH)
	if err != nil {
		return "", ContextError(err)
	}
	return hex.EncodeToString(randomId), nil
}

// NewServerContext makes the tunnelled handshake request to the Psiphon server
// and returns a ServerContext struct for use with subsequent Psiphon server API
// requests (e.g., periodic connected and status requests).
func NewServerContext(tunnel *Tunnel, sessionId string) (*ServerContext, error) {

	// For legacy servers, set up psiphonHttpsClient for
	// accessing the Psiphon API via the web service.
	var psiphonHttpsClient *http.Client
	if !tunnel.serverEntry.SupportsSSHAPIRequests() {
		var err error
		psiphonHttpsClient, err = makePsiphonHttpsClient(tunnel)
		if err != nil {
			return nil, ContextError(err)
		}
	}

	serverContext := &ServerContext{
		sessionId:          sessionId,
		tunnelNumber:       atomic.AddInt64(&nextTunnelNumber, 1),
		tunnel:             tunnel,
		psiphonHttpsClient: psiphonHttpsClient,
	}

	err := serverContext.doHandshakeRequest()
	if err != nil {
		return nil, ContextError(err)
	}

	return serverContext, nil
}

// doHandshakeRequest performs the "handshake" API request. The handshake
// returns upgrade info, newly discovered server entries -- which are
// stored -- and sponsor info (home pages, stat regexes).
func (serverContext *ServerContext) doHandshakeRequest() error {

	params := serverContext.getBaseParams()

	// *TODO*: this is obsolete?
	/*
		serverEntryIpAddresses, err := GetServerEntryIpAddresses()
		if err != nil {
			return ContextError(err)
		}

		// Submit a list of known servers -- this will be used for
		// discovery statistics.
		for _, ipAddress := range serverEntryIpAddresses {
			params = append(params, requestParam{"known_server", ipAddress})
		}
	*/

	var response []byte
	if serverContext.psiphonHttpsClient == nil {

		request, err := makeSSHAPIRequestPayload(params)
		if err != nil {
			return ContextError(err)
		}

		response, err = serverContext.tunnel.SendAPIRequest(
			SERVER_API_HANDSHAKE_REQUEST_NAME, request)
		if err != nil {
			return ContextError(err)
		}

	} else {

		// Legacy web service API request

		responseBody, err := serverContext.doGetRequest(
			makeRequestUrl(serverContext.tunnel, "", "handshake", params))
		if err != nil {
			return ContextError(err)
		}
		// Skip legacy format lines and just parse the JSON config line
		configLinePrefix := []byte("Config: ")
		for _, line := range bytes.Split(responseBody, []byte("\n")) {
			if bytes.HasPrefix(line, configLinePrefix) {
				response = line[len(configLinePrefix):]
				break
			}
		}
		if len(response) == 0 {
			return ContextError(errors.New("no config line found"))
		}
	}

	// Note:
	// - 'preemptive_reconnect_lifetime_milliseconds' is currently unused
	// - 'ssh_session_id' is ignored; client session ID is used instead
	var handshakeResponse struct {
		Homepages                  []string            `json:"homepages"`
		UpgradeClientVersion       string              `json:"upgrade_client_version"`
		PageViewRegexes            []map[string]string `json:"page_view_regexes"`
		HttpsRequestRegexes        []map[string]string `json:"https_request_regexes"`
		EncodedServerList          []string            `json:"encoded_server_list"`
		ClientRegion               string              `json:"client_region"`
		ServerTimestamp            string              `json:"server_timestamp"`
		ClientVerificationRequired bool                `json:"client_verification_required"`
	}
	err := json.Unmarshal(response, &handshakeResponse)
	if err != nil {
		return ContextError(err)
	}

	serverContext.clientRegion = handshakeResponse.ClientRegion
	NoticeClientRegion(serverContext.clientRegion)

	var decodedServerEntries []*ServerEntry

	// Store discovered server entries
	// We use the server's time, as it's available here, for the server entry
	// timestamp since this is more reliable than the client time.
	for _, encodedServerEntry := range handshakeResponse.EncodedServerList {

		serverEntry, err := DecodeServerEntry(
			encodedServerEntry,
			TruncateTimestampToHour(handshakeResponse.ServerTimestamp),
			SERVER_ENTRY_SOURCE_DISCOVERY)
		if err != nil {
			return ContextError(err)
		}

		err = ValidateServerEntry(serverEntry)
		if err != nil {
			// Skip this entry and continue with the next one
			continue
		}

		decodedServerEntries = append(decodedServerEntries, serverEntry)
	}

	// The reason we are storing the entire array of server entries at once rather
	// than one at a time is that some desirable side-effects get triggered by
	// StoreServerEntries that don't get triggered by StoreServerEntry.
	err = StoreServerEntries(decodedServerEntries, true)
	if err != nil {
		return ContextError(err)
	}

	// TODO: formally communicate the sponsor and upgrade info to an
	// outer client via some control interface.
	for _, homepage := range handshakeResponse.Homepages {
		NoticeHomepage(homepage)
	}

	serverContext.clientUpgradeVersion = handshakeResponse.UpgradeClientVersion
	if handshakeResponse.UpgradeClientVersion != "" {
		NoticeClientUpgradeAvailable(handshakeResponse.UpgradeClientVersion)
	} else {
		NoticeClientIsLatestVersion("")
	}

	var regexpsNotices []string
	serverContext.statsRegexps, regexpsNotices = transferstats.MakeRegexps(
		handshakeResponse.PageViewRegexes,
		handshakeResponse.HttpsRequestRegexes)

	for _, notice := range regexpsNotices {
		NoticeAlert(notice)
	}

	serverContext.serverHandshakeTimestamp = handshakeResponse.ServerTimestamp

	if handshakeResponse.ClientVerificationRequired {
		NoticeClientVerificationRequired()
	}

	return nil
}

// DoConnectedRequest performs the "connected" API request. This request is
// used for statistics. The server returns a last_connected token for
// the client to store and send next time it connects. This token is
// a timestamp (using the server clock, and should be rounded to the
// nearest hour) which is used to determine when a connection represents
// a unique user for a time period.
func (serverContext *ServerContext) DoConnectedRequest() error {

	params := serverContext.getBaseParams()

	const DATA_STORE_LAST_CONNECTED_KEY = "lastConnected"
	lastConnected, err := GetKeyValue(DATA_STORE_LAST_CONNECTED_KEY)
	if err != nil {
		return ContextError(err)
	}
	if lastConnected == "" {
		lastConnected = "None"
	}

	params["last_connected"] = lastConnected

	var response []byte
	if serverContext.psiphonHttpsClient == nil {

		request, err := makeSSHAPIRequestPayload(params)
		if err != nil {
			return ContextError(err)
		}

		response, err = serverContext.tunnel.SendAPIRequest(
			SERVER_API_CONNECTED_REQUEST_NAME, request)
		if err != nil {
			return ContextError(err)
		}

	} else {

		// Legacy web service API request

		response, err = serverContext.doGetRequest(
			makeRequestUrl(serverContext.tunnel, "", "connected", params))
		if err != nil {
			return ContextError(err)
		}
	}

	var connectedResponse struct {
		ConnectedTimestamp string `json:"connected_timestamp"`
	}
	err = json.Unmarshal(response, &connectedResponse)
	if err != nil {
		return ContextError(err)
	}

	err = SetKeyValue(
		DATA_STORE_LAST_CONNECTED_KEY, connectedResponse.ConnectedTimestamp)
	if err != nil {
		return ContextError(err)
	}
	return nil
}

// StatsRegexps gets the Regexps used for the statistics for this tunnel.
func (serverContext *ServerContext) StatsRegexps() *transferstats.Regexps {
	return serverContext.statsRegexps
}

// DoStatusRequest makes a "status" API request to the server, sending session stats.
func (serverContext *ServerContext) DoStatusRequest(tunnel *Tunnel) error {

	params := serverContext.getStatusParams(true)

	// Note: ensure putBackStatusRequestPayload is called, to replace
	// payload for future attempt, in all failure cases.

	statusPayload, statusPayloadInfo, err := makeStatusRequestPayload(
		tunnel.serverEntry.IpAddress)
	if err != nil {
		return ContextError(err)
	}

	if serverContext.psiphonHttpsClient == nil {

		rawMessage := json.RawMessage(statusPayload)
		params["statusData"] = &rawMessage

		var request []byte
		request, err = makeSSHAPIRequestPayload(params)

		if err == nil {
			_, err = serverContext.tunnel.SendAPIRequest(
				SERVER_API_STATUS_REQUEST_NAME, request)
		}

	} else {

		// Legacy web service API request
		err = serverContext.doPostRequest(
			makeRequestUrl(serverContext.tunnel, "", "status", params),
			"application/json",
			bytes.NewReader(statusPayload))
	}

	if err != nil {

		// Resend the transfer stats and tunnel stats later
		// Note: potential duplicate reports if the server received and processed
		// the request but the client failed to receive the response.
		putBackStatusRequestPayload(statusPayloadInfo)

		return ContextError(err)
	}

	confirmStatusRequestPayload(statusPayloadInfo)

	return nil
}

func (serverContext *ServerContext) getStatusParams(isTunneled bool) requestJSONObject {

	params := serverContext.getBaseParams()

	// Add a random amount of padding to help prevent stats updates from being
	// a predictable size (which often happens when the connection is quiet).
	// TODO: base64 encoding of padding means the padding size is not exactly
	// [0, PADDING_MAX_BYTES].

	randomPadding := MakeSecureRandomPadding(0, PSIPHON_API_STATUS_REQUEST_PADDING_MAX_BYTES)
	params["padding"] = base64.StdEncoding.EncodeToString(randomPadding)

	// Legacy clients set "connected" to "0" when disconnecting, and this value
	// is used to calculate session duration estimates. This is now superseded
	// by explicit tunnel stats duration reporting.
	// The legacy method of reconstructing session durations is not compatible
	// with this client's connected request retries and asynchronous final
	// status request attempts. So we simply set this "connected" flag to reflect
	// whether the request is sent tunneled or not.

	connected := "1"
	if !isTunneled {
		connected = "0"
	}
	params["connected"] = connected

	return params
}

// statusRequestPayloadInfo is a temporary structure for data used to
// either "clear" or "put back" status request payload data depending
// on whether or not the request succeeded.
type statusRequestPayloadInfo struct {
	serverId      string
	transferStats *transferstats.AccumulatedStats
	tunnelStats   [][]byte
}

func makeStatusRequestPayload(
	serverId string) ([]byte, *statusRequestPayloadInfo, error) {

	transferStats := transferstats.TakeOutStatsForServer(serverId)
	tunnelStats, err := TakeOutUnreportedTunnelStats(
		PSIPHON_API_TUNNEL_STATS_MAX_COUNT)
	if err != nil {
		NoticeAlert(
			"TakeOutUnreportedTunnelStats failed: %s", ContextError(err))
		tunnelStats = nil
		// Proceed with transferStats only
	}
	payloadInfo := &statusRequestPayloadInfo{
		serverId, transferStats, tunnelStats}

	payload := make(map[string]interface{})

	hostBytes, bytesTransferred := transferStats.GetStatsForStatusRequest()
	payload["host_bytes"] = hostBytes
	payload["bytes_transferred"] = bytesTransferred

	// We're not recording these fields, but the server requires them.
	payload["page_views"] = make([]string, 0)
	payload["https_requests"] = make([]string, 0)

	// Tunnel stats records are already in JSON format
	jsonTunnelStats := make([]json.RawMessage, len(tunnelStats))
	for i, tunnelStatsRecord := range tunnelStats {
		jsonTunnelStats[i] = json.RawMessage(tunnelStatsRecord)
	}
	payload["tunnel_stats"] = jsonTunnelStats

	jsonPayload, err := json.Marshal(payload)
	if err != nil {

		// Send the transfer stats and tunnel stats later
		putBackStatusRequestPayload(payloadInfo)

		return nil, nil, ContextError(err)
	}

	return jsonPayload, payloadInfo, nil
}

func putBackStatusRequestPayload(payloadInfo *statusRequestPayloadInfo) {
	transferstats.PutBackStatsForServer(
		payloadInfo.serverId, payloadInfo.transferStats)
	err := PutBackUnreportedTunnelStats(payloadInfo.tunnelStats)
	if err != nil {
		// These tunnel stats records won't be resent under after a
		// datastore re-initialization.
		NoticeAlert(
			"PutBackUnreportedTunnelStats failed: %s", ContextError(err))
	}
}

func confirmStatusRequestPayload(payloadInfo *statusRequestPayloadInfo) {
	err := ClearReportedTunnelStats(payloadInfo.tunnelStats)
	if err != nil {
		// These tunnel stats records may be resent.
		NoticeAlert(
			"ClearReportedTunnelStats failed: %s", ContextError(err))
	}
}

// TryUntunneledStatusRequest makes direct connections to the specified
// server (if supported) in an attempt to send useful bytes transferred
// and tunnel duration stats after a tunnel has alreay failed.
// The tunnel is assumed to be closed, but its config, protocol, and
// context values must still be valid.
// TryUntunneledStatusRequest emits notices detailing failed attempts.
func (serverContext *ServerContext) TryUntunneledStatusRequest(isShutdown bool) error {

	for _, port := range serverContext.tunnel.serverEntry.GetUntunneledWebRequestPorts() {
		err := serverContext.doUntunneledStatusRequest(port, isShutdown)
		if err == nil {
			return nil
		}
		NoticeAlert("doUntunneledStatusRequest failed for %s:%s: %s",
			serverContext.tunnel.serverEntry.IpAddress, port, err)
	}

	return errors.New("all attempts failed")
}

// doUntunneledStatusRequest attempts an untunneled status request.
func (serverContext *ServerContext) doUntunneledStatusRequest(
	port string, isShutdown bool) error {

	tunnel := serverContext.tunnel

	certificate, err := DecodeCertificate(tunnel.serverEntry.WebServerCertificate)
	if err != nil {
		return ContextError(err)
	}

	timeout := time.Duration(*tunnel.config.PsiphonApiServerTimeoutSeconds) * time.Second

	dialConfig := tunnel.untunneledDialConfig

	if isShutdown {
		timeout = PSIPHON_API_SHUTDOWN_SERVER_TIMEOUT

		// Use a copy of DialConfig without pendingConns. This ensures
		// this request isn't interrupted/canceled. This measure should
		// be used only with the very short PSIPHON_API_SHUTDOWN_SERVER_TIMEOUT.
		dialConfig = new(DialConfig)
		*dialConfig = *tunnel.untunneledDialConfig
	}

	url := makeRequestUrl(tunnel, port, "status", serverContext.getStatusParams(false))

	httpClient, url, err := MakeUntunneledHttpsClient(
		dialConfig,
		certificate,
		url,
		timeout)
	if err != nil {
		return ContextError(err)
	}

	statusPayload, statusPayloadInfo, err := makeStatusRequestPayload(tunnel.serverEntry.IpAddress)
	if err != nil {
		return ContextError(err)
	}

	bodyType := "application/json"
	body := bytes.NewReader(statusPayload)

	response, err := httpClient.Post(url, bodyType, body)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP POST request failed with response code: %d", response.StatusCode)
	}
	if err != nil {

		// Resend the transfer stats and tunnel stats later
		// Note: potential duplicate reports if the server received and processed
		// the request but the client failed to receive the response.
		putBackStatusRequestPayload(statusPayloadInfo)

		// Trim this error since it may include long URLs
		return ContextError(TrimError(err))
	}
	confirmStatusRequestPayload(statusPayloadInfo)
	response.Body.Close()

	return nil
}

// RecordTunnelStats records a tunnel duration and bytes
// sent and received for subsequent reporting and quality
// analysis.
//
// Tunnel durations are precisely measured client-side
// and reported in status requests. As the duration is
// not determined until the tunnel is closed, tunnel
// stats records are stored in the persistent datastore
// and reported via subsequent status requests sent to any
// Psiphon server.
//
// Since the status request that reports a tunnel stats
// record is not necessarily handled by the same server, the
// tunnel stats records include the original server ID.
//
// Other fields that may change between tunnel stats recording
// and reporting include client geo data, propagation channel,
// sponsor ID, client version. These are not stored in the
// datastore (client region, in particular, since that would
// create an on-disk record of user location).
// TODO: the server could encrypt, with a nonce and key unknown to
// the client, a blob containing this data; return it in the
// handshake response; and the client could store and later report
// this blob with its tunnel stats records.
//
// Multiple "status" requests may be in flight at once (due
// to multi-tunnel, asynchronous final status retry, and
// aggressive status requests for pre-registered tunnels),
// To avoid duplicate reporting, tunnel stats records are
// "taken-out" by a status request and then "put back" in
// case the request fails.
//
// Note: since tunnel stats records have a globally unique
// identifier (sessionId + tunnelNumber), we could tolerate
// duplicate reporting and filter our duplicates on the
// server-side. Permitting duplicate reporting could increase
// the velocity of reporting (for example, both the asynchronous
// untunneled final status requests and the post-connected
// immediate startus requests could try to report the same tunnel
// stats).
// Duplicate reporting may also occur when a server receives and
// processes a status request but the client fails to receive
// the response.
func RecordTunnelStats(
	sessionId string,
	tunnelNumber int64,
	tunnelServerIpAddress string,
	serverHandshakeTimestamp, duration string,
	totalBytesSent, totalBytesReceived int64) error {

	tunnelStats := struct {
		SessionId                string `json:"session_id"`
		TunnelNumber             int64  `json:"tunnel_number"`
		TunnelServerIpAddress    string `json:"tunnel_server_ip_address"`
		ServerHandshakeTimestamp string `json:"server_handshake_timestamp"`
		Duration                 string `json:"duration"`
		TotalBytesSent           int64  `json:"total_bytes_sent"`
		TotalBytesReceived       int64  `json:"total_bytes_received"`
	}{
		sessionId,
		tunnelNumber,
		tunnelServerIpAddress,
		serverHandshakeTimestamp,
		duration,
		totalBytesSent,
		totalBytesReceived,
	}

	tunnelStatsJson, err := json.Marshal(tunnelStats)
	if err != nil {
		return ContextError(err)
	}

	return StoreTunnelStats(tunnelStatsJson)
}

// DoClientVerificationRequest performs the "client_verification" API
// request. This request is used to verify that the client is a valid
// Psiphon client, which will determine how the server treats the client
// traffic. The proof-of-validity is platform-specific and the payload
// is opaque to this function but assumed to be JSON.
func (serverContext *ServerContext) DoClientVerificationRequest(
	verificationPayload string) error {

	params := serverContext.getBaseParams()

	if serverContext.psiphonHttpsClient == nil {

		rawMessage := json.RawMessage(verificationPayload)
		params["verificationData"] = &rawMessage

		request, err := makeSSHAPIRequestPayload(params)
		if err != nil {
			return ContextError(err)
		}

		_, err = serverContext.tunnel.SendAPIRequest(
			SERVER_API_CLIENT_VERIFICATION_REQUEST_NAME, request)
		if err != nil {
			return ContextError(err)
		}

	} else {

		// Legacy web service API request
		err := serverContext.doPostRequest(
			makeRequestUrl(serverContext.tunnel, "", "client_verification", params),
			"application/json",
			bytes.NewReader([]byte(verificationPayload)))
		if err != nil {
			return ContextError(err)
		}
	}

	return nil
}

// doGetRequest makes a tunneled HTTPS request and returns the response body.
func (serverContext *ServerContext) doGetRequest(
	requestUrl string) (responseBody []byte, err error) {

	response, err := serverContext.psiphonHttpsClient.Get(requestUrl)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP GET request failed with response code: %d", response.StatusCode)
	}
	if err != nil {
		// Trim this error since it may include long URLs
		return nil, ContextError(TrimError(err))
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, ContextError(err)
	}
	return body, nil
}

// doPostRequest makes a tunneled HTTPS POST request.
func (serverContext *ServerContext) doPostRequest(
	requestUrl string, bodyType string, body io.Reader) (err error) {

	response, err := serverContext.psiphonHttpsClient.Post(requestUrl, bodyType, body)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP POST request failed with response code: %d", response.StatusCode)
	}
	if err != nil {
		// Trim this error since it may include long URLs
		return ContextError(TrimError(err))
	}
	response.Body.Close()
	return nil
}

type requestJSONObject map[string]interface{}

// getBaseParams returns all the common API parameters that are included
// with each Psiphon API request. These common parameters are used for
// statistics.
func (serverContext *ServerContext) getBaseParams() requestJSONObject {

	params := make(requestJSONObject)

	tunnel := serverContext.tunnel

	params["session_id"] = serverContext.sessionId
	params["client_session_id"] = serverContext.sessionId
	params["server_secret"] = tunnel.serverEntry.WebServerSecret
	params["propagation_channel_id"] = tunnel.config.PropagationChannelId
	params["sponsor_id"] = tunnel.config.SponsorId
	params["client_version"] = tunnel.config.ClientVersion
	// TODO: client_tunnel_core_version?
	params["relay_protocol"] = tunnel.protocol
	params["client_platform"] = tunnel.config.ClientPlatform
	params["tunnel_whole_device"] = strconv.Itoa(tunnel.config.TunnelWholeDevice)

	// The following parameters may be blank and must
	// not be sent to the server if blank.

	if tunnel.config.DeviceRegion != "" {
		params["device_region"] = tunnel.config.DeviceRegion
	}
	if tunnel.meekStats != nil {
		if tunnel.meekStats.DialAddress != "" {
			params["meek_dial_address"] = tunnel.meekStats.DialAddress
		}
		if tunnel.meekStats.ResolvedIPAddress != "" {
			params["meek_resolved_ip_address"] = tunnel.meekStats.ResolvedIPAddress
		}
		if tunnel.meekStats.SNIServerName != "" {
			params["meek_sni_server_name"] = tunnel.meekStats.SNIServerName
		}
		if tunnel.meekStats.HostHeader != "" {
			params["meek_host_header"] = tunnel.meekStats.HostHeader
		}
		transformedHostName := "0"
		if tunnel.meekStats.TransformedHostName {
			transformedHostName = "1"
		}
		params["meek_transformed_host_name"] = transformedHostName
	}

	if tunnel.serverEntry.Region != "" {
		params["server_entry_region"] = tunnel.serverEntry.Region
	}

	if tunnel.serverEntry.LocalSource != "" {
		params["server_entry_source"] = tunnel.serverEntry.LocalSource
	}

	// As with last_connected, this timestamp stat, which may be
	// a precise handshake request server timestamp, is truncated
	// to hour granularity to avoid introducing a reconstructable
	// cross-session user trace into server logs.
	localServerEntryTimestamp := TruncateTimestampToHour(tunnel.serverEntry.LocalTimestamp)
	if localServerEntryTimestamp != "" {
		params["server_entry_timestamp"] = localServerEntryTimestamp
	}

	return params
}

// makeSSHAPIRequestPayload makes a JSON payload for an SSH API request.
func makeSSHAPIRequestPayload(params requestJSONObject) ([]byte, error) {
	jsonPayload, err := json.Marshal(params)
	if err != nil {
		return nil, ContextError(err)
	}
	return jsonPayload, nil
}

// makeRequestUrl makes a URL for a web service API request.
func makeRequestUrl(tunnel *Tunnel, port, path string, params requestJSONObject) string {
	var requestUrl bytes.Buffer

	if port == "" {
		port = tunnel.serverEntry.WebServerPort
	}

	// Note: don't prefix with HTTPS scheme, see comment in doGetRequest.
	// e.g., don't do this: requestUrl.WriteString("https://")
	requestUrl.WriteString("http://")
	requestUrl.WriteString(tunnel.serverEntry.IpAddress)
	requestUrl.WriteString(":")
	requestUrl.WriteString(port)
	requestUrl.WriteString("/")
	requestUrl.WriteString(path)
	firstParam := true
	for name, value := range params {
		if strValue, ok := value.(string); ok {
			if firstParam {
				requestUrl.WriteString("?")
				firstParam = false
			} else {
				requestUrl.WriteString("&")
			}
			requestUrl.WriteString(name)
			requestUrl.WriteString("=")
			requestUrl.WriteString(strValue)
		}
	}
	return requestUrl.String()
}

// makePsiphonHttpsClient creates a Psiphon HTTPS client that tunnels web service API
// requests and which validates the web server using the Psiphon server entry web server
// certificate. This is not a general purpose HTTPS client.
// As the custom dialer makes an explicit TLS connection, URLs submitted to the returned
// http.Client should use the "http://" scheme. Otherwise http.Transport will try to do another TLS
// handshake inside the explicit TLS session.
func makePsiphonHttpsClient(tunnel *Tunnel) (httpsClient *http.Client, err error) {
	certificate, err := DecodeCertificate(tunnel.serverEntry.WebServerCertificate)
	if err != nil {
		return nil, ContextError(err)
	}
	tunneledDialer := func(_, addr string) (conn net.Conn, err error) {
		// TODO: check tunnel.isClosed, and apply TUNNEL_PORT_FORWARD_DIAL_TIMEOUT as in Tunnel.Dial?
		return tunnel.sshClient.Dial("tcp", addr)
	}
	timeout := time.Duration(*tunnel.config.PsiphonApiServerTimeoutSeconds) * time.Second
	dialer := NewCustomTLSDialer(
		&CustomTLSConfig{
			Dial:                    tunneledDialer,
			Timeout:                 timeout,
			VerifyLegacyCertificate: certificate,
		})
	transport := &http.Transport{
		Dial: dialer,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
