// Package upnp provides UPnP IGD port mapping for NAT traversal.
//
// It discovers the local UPnP gateway via SSDP, finds the WANIPConnection
// or WANPPPConnection service, and issues SOAP calls to map ports.
// Mappings are renewed automatically until the context is cancelled.
package upnp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

const (
	ssdpAddr         = "239.255.255.250:1900"
	ssdpMulticastIP  = "239.255.255.250"
	ssdpPort         = 1900
	discoveryTimeout = 3 * time.Second
	leaseDuration    = 3600 // seconds
	renewInterval    = 20 * time.Minute
	userAgent        = "bittorrent/1.0"
)

// Service URNs we look for in the device tree.
var serviceURNs = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:2",
}

// MapPort discovers the UPnP gateway and maps internalPort on the given protocol.
// protocol must be "TCP" or "UDP".
// Returns the external IP, actual mapped port, and a cleanup function.
// The mapping is renewed automatically until ctx is cancelled or cleanup is called.
func MapPort(ctx context.Context, protocol string, internalPort int, description string) (externalIP net.IP, externalPort int, cleanup func(), err error) {
	// Try UPnP IGD first (most common on consumer routers).
	ip, port, cl, err := mapPortUPnP(ctx, protocol, internalPort, description)
	if err == nil {
		return ip, port, cl, nil
	}
	upnpErr := err

	// Try PCP (RFC 6887) — modern successor to NAT-PMP.
	ip, port, cl, err = mapPortPCP(ctx, protocol, internalPort, description)
	if err == nil {
		return ip, port, cl, nil
	}
	pcpErr := err

	// Fall back to NAT-PMP (RFC 6886) — simpler legacy protocol.
	log.Debug("UPnP and PCP failed, trying NAT-PMP", "sub", "upnp", "upnpErr", upnpErr, "pcpErr", pcpErr)
	return mapPortNATPMP(ctx, protocol, internalPort, description)
}

// mapPortUPnP implements UPnP IGD port mapping.
func mapPortUPnP(ctx context.Context, protocol string, internalPort int, description string) (externalIP net.IP, externalPort int, cleanup func(), err error) {
	loc, err := discover(ctx)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("SSDP discovery: %w", err)
	}
	log.Debug("discovered gateway", "sub", "upnp", "location", loc)

	serviceURL, err := getServiceURL(ctx, loc)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("service URL: %w", err)
	}
	log.Debug("found service", "sub", "upnp", "url", serviceURL)

	// Determine our local IP facing the gateway.
	gwURL, err := url.Parse(loc)
	if err != nil {
		return nil, 0, nil, err
	}
	localIP, err := localIPForGateway(gwURL.Hostname())
	if err != nil {
		return nil, 0, nil, fmt.Errorf("local IP: %w", err)
	}

	externalPort = internalPort

	// Initial mapping.
	if err := addPortMapping(ctx, serviceURL, protocol, externalPort, localIP.String(), internalPort, description); err != nil {
		return nil, 0, nil, fmt.Errorf("add port mapping: %w", err)
	}
	log.Info("port mapped", "sub", "upnp", "proto", protocol, "ext", externalPort, "int", internalPort)

	// Get external IP (best-effort).
	extIP, _ := getExternalIP(ctx, serviceURL)

	// Renewal + cleanup.
	renewCtx, cancel := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})

	go func() {
		defer close(cleanupDone)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := addPortMapping(renewCtx, serviceURL, protocol, externalPort, localIP.String(), internalPort, description); err != nil {
					log.Warn("UPnP renewal failed", "sub", "upnp", "proto", protocol, "err", err)
				} else {
					log.Debug("UPnP renewed", "sub", "upnp", "proto", protocol, "port", externalPort)
				}
			case <-renewCtx.Done():
				// Best-effort delete with a short timeout.
				delCtx, delCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = deletePortMapping(delCtx, serviceURL, protocol, externalPort)
				delCancel()
				log.Debug("port mapping deleted", "sub", "upnp", "proto", protocol, "port", externalPort)
				return
			}
		}
	}()

	cleanupFn := func() {
		cancel()
		<-cleanupDone
	}

	return extIP, externalPort, cleanupFn, nil
}

// discover sends an SSDP M-SEARCH multicast and returns the Location URL
// of the first IGD that responds.
func discover(ctx context.Context) (string, error) {
	search := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 3\r\n" +
		"USER-AGENT: " + userAgent + "\r\n" +
		"\r\n"

	addr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return "", err
	}

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	deadline := time.Now().Add(discoveryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.WriteTo([]byte(search), addr); err != nil {
		return "", err
	}

	buf := make([]byte, 65536)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", fmt.Errorf("no UPnP gateway found: %w", err)
		}

		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		resp.Body.Close()

		location := resp.Header.Get("Location")
		if location != "" {
			return location, nil
		}
	}
}

// XML types for device description parsing.
type xmlRoot struct {
	Device xmlDevice `xml:"device"`
}

type xmlDevice struct {
	DeviceType string       `xml:"deviceType"`
	Devices    []xmlDevice  `xml:"deviceList>device"`
	Services   []xmlService `xml:"serviceList>service"`
}

type xmlService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// getServiceURL fetches the device description XML and walks the device tree
// to find the WANIPConnection or WANPPPConnection service control URL.
func getServiceURL(ctx context.Context, location string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("device description: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var root xmlRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("XML parse: %w", err)
	}

	baseURL, err := url.Parse(location)
	if err != nil {
		return "", err
	}

	// Walk the device tree to find a matching service.
	controlURL := findServiceURL(root.Device)
	if controlURL == "" {
		return "", errors.New("no WANIPConnection or WANPPPConnection service found")
	}

	// Resolve relative control URL against the base.
	resolved, err := baseURL.Parse(controlURL)
	if err != nil {
		return "", err
	}

	return resolved.String(), nil
}

// findServiceURL recursively searches the device tree for a known service URN.
func findServiceURL(dev xmlDevice) string {
	for _, svc := range dev.Services {
		for _, urn := range serviceURNs {
			if svc.ServiceType == urn {
				return svc.ControlURL
			}
		}
	}
	for _, child := range dev.Devices {
		if u := findServiceURL(child); u != "" {
			return u
		}
	}
	return ""
}

// SOAP envelope template.
const soapEnvelope = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body>%s</s:Body>
</s:Envelope>`

// addPortMapping issues the SOAP AddPortMapping call.
func addPortMapping(ctx context.Context, serviceURL, protocol string, externalPort int, internalClient string, internalPort int, description string) error {
	body := fmt.Sprintf(
		`<u:AddPortMapping xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`+
			`<NewRemoteHost></NewRemoteHost>`+
			`<NewExternalPort>%d</NewExternalPort>`+
			`<NewProtocol>%s</NewProtocol>`+
			`<NewInternalPort>%d</NewInternalPort>`+
			`<NewInternalClient>%s</NewInternalClient>`+
			`<NewEnabled>1</NewEnabled>`+
			`<NewPortMappingDescription>%s</NewPortMappingDescription>`+
			`<NewLeaseDuration>%d</NewLeaseDuration>`+
			`</u:AddPortMapping>`,
		externalPort, protocol, internalPort, internalClient, description, leaseDuration,
	)
	_, err := soapRequest(ctx, serviceURL, "WANIPConnection:1", "AddPortMapping", body)
	return err
}

// deletePortMapping issues the SOAP DeletePortMapping call.
func deletePortMapping(ctx context.Context, serviceURL, protocol string, externalPort int) error {
	body := fmt.Sprintf(
		`<u:DeletePortMapping xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`+
			`<NewRemoteHost></NewRemoteHost>`+
			`<NewExternalPort>%d</NewExternalPort>`+
			`<NewProtocol>%s</NewProtocol>`+
			`</u:DeletePortMapping>`,
		externalPort, protocol,
	)
	_, err := soapRequest(ctx, serviceURL, "WANIPConnection:1", "DeletePortMapping", body)
	return err
}

// SOAP response types.
type soapGetExternalIPEnvelope struct {
	XMLName xml.Name              `xml:"Envelope"`
	Body    soapGetExternalIPBody `xml:"Body"`
}

type soapGetExternalIPBody struct {
	Response soapGetExternalIPResponse `xml:"GetExternalIPAddressResponse"`
}

type soapGetExternalIPResponse struct {
	ExternalIP string `xml:"NewExternalIPAddress"`
}

type soapErrorResponse struct {
	ErrorCode        int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
	ErrorDescription string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
}

// getExternalIP queries the IGD for its external IP address.
func getExternalIP(ctx context.Context, serviceURL string) (net.IP, error) {
	body := `<u:GetExternalIPAddress xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1" />`
	resp, err := soapRequest(ctx, serviceURL, "WANIPConnection:1", "GetExternalIPAddress", body)
	if err != nil {
		return nil, err
	}

	var envelope soapGetExternalIPEnvelope
	if err := xml.Unmarshal(resp, &envelope); err != nil {
		return nil, err
	}

	ip := net.ParseIP(envelope.Body.Response.ExternalIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid external IP: %q", envelope.Body.Response.ExternalIP)
	}
	return ip, nil
}

// localIPForGateway determines the local IP used to reach the given gateway host.
func localIPForGateway(gatewayHost string) (net.IP, error) {
	// Dial UDP (no actual traffic) to discover which local IP routes to the gateway.
	conn, err := net.DialTimeout("udp4", net.JoinHostPort(gatewayHost, "1"), time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP, nil
}

// soapRequest sends a SOAP POST to the given URL and returns the response body.
func soapRequest(ctx context.Context, serviceURL, service, action, body string) ([]byte, error) {
	envelope := fmt.Sprintf(soapEnvelope, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL, strings.NewReader(envelope))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("User-Agent", userAgent)
	req.Header["SOAPAction"] = []string{fmt.Sprintf(`"urn:schemas-upnp-org:service:%s#%s"`, service, action)}
	req.Header.Set("Connection", "Close")

	log.Debug("SOAP request", "sub", "upnp", "action", action, "url", serviceURL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		// Try to parse a SOAP error for a better message.
		var soapErr soapErrorResponse
		if xml.Unmarshal(respBody, &soapErr) == nil && soapErr.ErrorCode != 0 {
			return respBody, fmt.Errorf("%s: UPnP error %d: %s", action, soapErr.ErrorCode, soapErr.ErrorDescription)
		}
		return respBody, fmt.Errorf("%s: HTTP %s", action, resp.Status)
	}

	return respBody, nil
}
