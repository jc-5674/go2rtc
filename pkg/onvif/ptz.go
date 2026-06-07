package onvif

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// PTZ service operation names. None collide with the Device/Media operations
// the router already handles (dispatch is by SOAP action name), so they slot
// straight into the same switch.
const (
	PTZGetServiceCapabilities  = "GetServiceCapabilities" // shared name; handled as media today
	PTZGetConfigurations       = "GetConfigurations"
	PTZGetConfiguration        = "GetConfiguration"
	PTZGetConfigurationOptions = "GetConfigurationOptions"
	PTZGetNodes                = "GetNodes"
	PTZGetNode                 = "GetNode"
	PTZGetStatus               = "GetStatus"
	PTZContinuousMove          = "ContinuousMove"
	PTZRelativeMove            = "RelativeMove"
	PTZAbsoluteMove            = "AbsoluteMove"
	PTZStop                    = "Stop"
	PTZGetPresets              = "GetPresets"
	PTZGotoPreset              = "GotoPreset"
	PTZSetPreset               = "SetPreset"
	PTZRemovePreset            = "RemovePreset"
	PTZGotoHomePosition        = "GotoHomePosition"
)

// One synthetic PTZ node shared by every relayed profile. Nx only needs it to
// learn the velocity spaces; the real motion limits live on the camera and are
// honoured there. Generic -1..1 velocity space is what virtually every camera
// accepts for ContinuousMove.
const PTZNodeToken = "go2rtc_ptz_node"

const (
	spacePTVel = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"
	spaceZVel  = "http://www.onvif.org/ver10/tptz/ZoomSpaces/VelocityGenericSpace"
)

func ptzConfigToken(profileName string) string { return "ptzcfg_" + profileName }

// PTZConfigurationXML is the <tt:PTZConfiguration> block embedded inside a media
// Profile (GetProfiles) for a PTZ-enabled profile — this is what makes a strict
// VMS show PTZ controls for the channel.
func PTZConfigurationXML(profileName string) string {
	return `    <tt:PTZConfiguration token="` + ptzConfigToken(profileName) + `">
        <tt:Name>PTZ</tt:Name>
        <tt:UseCount>1</tt:UseCount>
        <tt:NodeToken>` + PTZNodeToken + `</tt:NodeToken>
        <tt:DefaultContinuousPanTiltVelocitySpace>` + spacePTVel + `</tt:DefaultContinuousPanTiltVelocitySpace>
        <tt:DefaultContinuousZoomVelocitySpace>` + spaceZVel + `</tt:DefaultContinuousZoomVelocitySpace>
        <tt:DefaultPTZTimeout>PT5S</tt:DefaultPTZTimeout>
    </tt:PTZConfiguration>
`
}

// ----- synthesised server-side PTZ metadata (go2rtc tokens, generic spaces) -----

func ptzConfigurationBody(profileName string) string {
	return `<tptz:PTZConfiguration token="` + ptzConfigToken(profileName) + `">
    <tt:Name>PTZ</tt:Name>
    <tt:UseCount>1</tt:UseCount>
    <tt:NodeToken>` + PTZNodeToken + `</tt:NodeToken>
    <tt:DefaultContinuousPanTiltVelocitySpace>` + spacePTVel + `</tt:DefaultContinuousPanTiltVelocitySpace>
    <tt:DefaultContinuousZoomVelocitySpace>` + spaceZVel + `</tt:DefaultContinuousZoomVelocitySpace>
    <tt:DefaultPTZTimeout>PT5S</tt:DefaultPTZTimeout>
</tptz:PTZConfiguration>
`
}

func GetPTZConfigurationsResponse(profiles []OnvifProfile) []byte {
	e := NewEnvelope()
	e.Append(`<tptz:GetConfigurationsResponse>
`)
	for _, p := range profiles {
		if p.Ptz {
			e.Append(ptzConfigurationBody(p.Name))
		}
	}
	e.Append(`</tptz:GetConfigurationsResponse>`)
	return e.Bytes()
}

func GetPTZConfigurationResponse(profileName string) []byte {
	e := NewEnvelope()
	e.Append(`<tptz:GetConfigurationResponse>
`, ptzConfigurationBody(profileName), `</tptz:GetConfigurationResponse>`)
	return e.Bytes()
}

func ptzNodeBody() string {
	return `<tptz:PTZNode token="` + PTZNodeToken + `" FixedHomePosition="false" GeoMove="false">
    <tt:Name>go2rtc PTZ</tt:Name>
    <tt:SupportedPTZSpaces>
        <tt:ContinuousPanTiltVelocitySpace>
            <tt:URI>` + spacePTVel + `</tt:URI>
            <tt:XRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:XRange>
            <tt:YRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:YRange>
        </tt:ContinuousPanTiltVelocitySpace>
        <tt:ContinuousZoomVelocitySpace>
            <tt:URI>` + spaceZVel + `</tt:URI>
            <tt:XRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:XRange>
        </tt:ContinuousZoomVelocitySpace>
    </tt:SupportedPTZSpaces>
    <tt:MaximumNumberOfPresets>255</tt:MaximumNumberOfPresets>
    <tt:HomeSupported>true</tt:HomeSupported>
</tptz:PTZNode>
`
}

func GetPTZNodesResponse() []byte {
	e := NewEnvelope()
	e.Append(`<tptz:GetNodesResponse>
`, ptzNodeBody(), `</tptz:GetNodesResponse>`)
	return e.Bytes()
}

func GetPTZNodeResponse() []byte {
	e := NewEnvelope()
	e.Append(`<tptz:GetNodeResponse>
`, ptzNodeBody(), `</tptz:GetNodeResponse>`)
	return e.Bytes()
}

func GetPTZConfigurationOptionsResponse() []byte {
	e := NewEnvelope()
	e.Append(`<tptz:GetConfigurationOptionsResponse>
    <tptz:PTZConfigurationOptions>
        <tt:Spaces>
            <tt:ContinuousPanTiltVelocitySpace>
                <tt:URI>` + spacePTVel + `</tt:URI>
                <tt:XRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:XRange>
                <tt:YRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:YRange>
            </tt:ContinuousPanTiltVelocitySpace>
            <tt:ContinuousZoomVelocitySpace>
                <tt:URI>` + spaceZVel + `</tt:URI>
                <tt:XRange><tt:Min>-1.0</tt:Min><tt:Max>1.0</tt:Max></tt:XRange>
            </tt:ContinuousZoomVelocitySpace>
        </tt:Spaces>
        <tt:PTZTimeout><tt:Min>PT1S</tt:Min><tt:Max>PT10S</tt:Max></tt:PTZTimeout>
    </tptz:PTZConfigurationOptions>
</tptz:GetConfigurationOptionsResponse>`)
	return e.Bytes()
}

func GetPTZStatusResponse() []byte {
	e := NewEnvelope()
	e.Appendf(`<tptz:GetStatusResponse>
    <tptz:PTZStatus>
        <tt:Position>
            <tt:PanTilt x="0.0" y="0.0"/>
            <tt:Zoom x="0.0"/>
        </tt:Position>
        <tt:MoveStatus><tt:PanTilt>IDLE</tt:PanTilt><tt:Zoom>IDLE</tt:Zoom></tt:MoveStatus>
        <tt:UtcTime>%s</tt:UtcTime>
    </tptz:PTZStatus>
</tptz:GetStatusResponse>`, time.Now().UTC().Format(time.RFC3339))
	return e.Bytes()
}

// ----- velocity parsing from an incoming Nx ContinuousMove -----

var (
	rePanTiltX = regexp.MustCompile(`PanTilt[^>]*\bx="([^"]+)"`)
	rePanTiltY = regexp.MustCompile(`PanTilt[^>]*\by="([^"]+)"`)
	reZoomX    = regexp.MustCompile(`Zoom[^>]*\bx="([^"]+)"`)
)

// ParsePTZVelocity extracts the pan/tilt/zoom velocity from a ContinuousMove body.
func ParsePTZVelocity(b []byte) (x, y, z float64) {
	s := string(b)
	if m := rePanTiltX.FindStringSubmatch(s); m != nil {
		x, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := rePanTiltY.FindStringSubmatch(s); m != nil {
		y, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reZoomX.FindStringSubmatch(s); m != nil {
		z, _ = strconv.ParseFloat(m[1], 64)
	}
	return
}

// ----- camera-facing PTZ calls (relayed to the backend camera) -----

func (c *Client) PTZContinuousMove(camToken string, x, y, z float64) ([]byte, error) {
	return c.Request(c.ptzURL, fmt.Sprintf(`<tptz:ContinuousMove>
    <tptz:ProfileToken>%s</tptz:ProfileToken>
    <tptz:Velocity>
        <tt:PanTilt x="%.4f" y="%.4f" space="%s"/>
        <tt:Zoom x="%.4f" space="%s"/>
    </tptz:Velocity>
</tptz:ContinuousMove>`, camToken, x, y, spacePTVel, z, spaceZVel))
}

func (c *Client) PTZStop(camToken string) ([]byte, error) {
	return c.Request(c.ptzURL, `<tptz:Stop>
    <tptz:ProfileToken>`+camToken+`</tptz:ProfileToken>
    <tptz:PanTilt>true</tptz:PanTilt>
    <tptz:Zoom>true</tptz:Zoom>
</tptz:Stop>`)
}

func (c *Client) PTZGetPresets(camToken string) ([]byte, error) {
	return c.Request(c.ptzURL, `<tptz:GetPresets><tptz:ProfileToken>`+camToken+`</tptz:ProfileToken></tptz:GetPresets>`)
}

func (c *Client) PTZGotoPreset(camToken, presetToken string) ([]byte, error) {
	return c.Request(c.ptzURL, `<tptz:GotoPreset>
    <tptz:ProfileToken>`+camToken+`</tptz:ProfileToken>
    <tptz:PresetToken>`+presetToken+`</tptz:PresetToken>
</tptz:GotoPreset>`)
}
