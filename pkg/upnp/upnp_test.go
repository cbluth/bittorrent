package upnp

import (
	"encoding/xml"
	"testing"
)

func TestParseDeviceXML(t *testing.T) {
	deviceXML := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <controlURL>/ctl/IPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`)

	var root xmlRoot
	if err := xml.Unmarshal(deviceXML, &root); err != nil {
		t.Fatal(err)
	}

	controlURL := findServiceURL(root.Device)
	if controlURL != "/ctl/IPConn" {
		t.Fatalf("expected /ctl/IPConn, got %q", controlURL)
	}
}

func TestParseDeviceXML_PPP(t *testing.T) {
	deviceXML := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType>
                <controlURL>/ctl/PPPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`)

	var root xmlRoot
	if err := xml.Unmarshal(deviceXML, &root); err != nil {
		t.Fatal(err)
	}

	controlURL := findServiceURL(root.Device)
	if controlURL != "/ctl/PPPConn" {
		t.Fatalf("expected /ctl/PPPConn, got %q", controlURL)
	}
}

func TestParseDeviceXML_NoService(t *testing.T) {
	deviceXML := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  </device>
</root>`)

	var root xmlRoot
	if err := xml.Unmarshal(deviceXML, &root); err != nil {
		t.Fatal(err)
	}

	controlURL := findServiceURL(root.Device)
	if controlURL != "" {
		t.Fatalf("expected empty, got %q", controlURL)
	}
}

func TestParseExternalIPResponse(t *testing.T) {
	soapResponse := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
		<s:Body>
			<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">
			<NewExternalIPAddress>203.0.113.42</NewExternalIPAddress>
			</u:GetExternalIPAddressResponse>
		</s:Body>
		</s:Envelope>`)

	var envelope soapGetExternalIPEnvelope
	if err := xml.Unmarshal(soapResponse, &envelope); err != nil {
		t.Fatal(err)
	}

	if envelope.Body.Response.ExternalIP != "203.0.113.42" {
		t.Fatalf("expected 203.0.113.42, got %q", envelope.Body.Response.ExternalIP)
	}
}

func TestParseSoapError(t *testing.T) {
	soapResponse := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
		<s:Body>
			<s:Fault>
				<faultcode>s:Client</faultcode>
				<faultstring>UPnPError</faultstring>
				<detail>
					<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
					<errorCode>725</errorCode>
					<errorDescription>OnlyPermanentLeasesSupported</errorDescription></UPnPError>
				</detail>
			</s:Fault>
		</s:Body>
		</s:Envelope>`)

	var envelope soapErrorResponse
	if err := xml.Unmarshal(soapResponse, &envelope); err != nil {
		t.Fatal(err)
	}

	if envelope.ErrorCode != 725 {
		t.Fatalf("expected error code 725, got %d", envelope.ErrorCode)
	}
	if envelope.ErrorDescription != "OnlyPermanentLeasesSupported" {
		t.Fatalf("expected OnlyPermanentLeasesSupported, got %q", envelope.ErrorDescription)
	}
}
