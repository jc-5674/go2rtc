package onvif

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/rtsp"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/rs/zerolog"
)

var OnvifProfiles []onvif.OnvifProfile

func Init() {
	var cfg struct {
		Onvif struct {
			OnvifProfiles []onvif.OnvifProfile `yaml:"profiles"`
		} `yaml:"onvif"`
	}

	app.LoadConfig(&cfg)
	OnvifProfiles = cfg.Onvif.OnvifProfiles

	log = app.GetLogger("onvif")

	// Auto-wire two-way-audio talk sinks before anything serves: derives each
	// talk-enabled profile's ISAPI endpoint from its existing stream source so
	// `two_way_audio: true` is a single self-contained flag (creds reused, never
	// restated). Safe here — streams.Init() ran earlier in main(), so the
	// configured streams already exist.
	wireTwoWayAudioSinks(OnvifProfiles)

	streams.HandleFunc("onvif", streamOnvif)

	// Main ONVIF server on the go2rtc API port — serves all profiles.
	api.HandleFunc("/onvif/", makeOnvifHandler(OnvifProfiles, api.Port, "go2rtc"))

	// ONVIF client autodiscovery endpoint
	api.HandleFunc("api/onvif", apiOnvif)

	// Per-camera ONVIF servers: each profile with port > 0 gets its own
	// HTTP server that acts as an independent ONVIF device. Register them
	// with the WS-Discovery system before starting the discovery server.
	for _, profile := range OnvifProfiles {
		if profile.Port <= 0 {
			continue
		}
		if len(profile.Streams) == 0 {
			log.Debug().Msgf("[onvif] skipping profile %s: no streams defined", profile.Name)
			continue
		}
		p := profile // capture loop variable
		var ip net.IP
		if p.IP != "" {
			ip = net.ParseIP(p.IP)
			if ip != nil {
				ip = ip.To4()
			}
			if ip == nil {
				log.Warn().Msgf("[onvif] invalid ip %q for profile %s, using all interfaces", p.IP, p.Name)
			}
		}
		uuid := onvif.RegisterDevice(p.Port, p.Name, ip)
		handler := makeOnvifHandler([]onvif.OnvifProfile{p}, api.Port, p.Name)
		go startCameraServer(ip, p.Port, uuid, handler)
	}

	// Include the main go2rtc device in WS-Discovery only when at least one
	// profile has no dedicated port — otherwise Unifi Protect would also
	// discover the generic "go2rtc" device and display its name pre-adoption.
	// Include the main go2rtc device only if there is at least one profile
	// that has streams but no dedicated port (it needs the main server).
	// Profiles with dedicated ports serve themselves; profiles with no streams
	// are inactive and should not cause anything to be advertised.
	includeMain := len(OnvifProfiles) == 0
	for _, p := range OnvifProfiles {
		if p.Port <= 0 && len(p.Streams) > 0 {
			includeMain = true
			break
		}
	}

	// WS-Discovery server (must start after all RegisterDevice calls).
	if err := onvif.StartDiscoveryServer(api.Port, "go2rtc", includeMain); err != nil {
		log.Warn().Err(err).Msg("[onvif] WS-Discovery server failed to start (port 3702 in use?)")
	} else {
		log.Info().Int("port", 3702).Msg("[onvif] WS-Discovery server listening")
	}
}

var log zerolog.Logger

// ----- PTZ relay: resolve the backend camera + cache one client per camera -----

type ptzCamera struct {
	client *onvif.Client
	token  string // the camera's own PTZ-capable profile token
}

var (
	ptzMu    sync.Mutex
	ptzCache = map[string]*ptzCamera{}
)

func anyPTZ(profiles []onvif.OnvifProfile) bool {
	for _, p := range profiles {
		if p.Ptz {
			return true
		}
	}
	return false
}

func anyTwoWayAudio(profiles []onvif.OnvifProfile) bool {
	for _, p := range profiles {
		if p.TwoWayAudio {
			return true
		}
	}
	return false
}

// resolveCameraISAPIURL derives the Hikvision ISAPI two-way-audio sink for a
// talk-enabled profile/stream — reusing the host + credentials already
// configured for the video source, so they are never restated. Order: explicit
// audio_url override, then derive from an rtsp/rtsps/http/https source (same
// host + creds, default ISAPI HTTP port). Returns "" if nothing is derivable.
func resolveCameraISAPIURL(p *onvif.OnvifProfile, streamName string) string {
	if p == nil || !p.TwoWayAudio {
		return ""
	}
	if p.AudioURL != "" {
		return p.AudioURL
	}
	st := streams.Get(streamName)
	if st == nil {
		return ""
	}
	for _, src := range st.Sources() {
		u, err := url.Parse(src)
		if err != nil || u.User == nil {
			continue
		}
		switch u.Scheme {
		case "rtsp", "rtsps", "http", "https":
			// Hostname() drops the source port (e.g. rtsp 554); ISAPI is HTTP and
			// isapi.Dial defaults to the standard port. Non-standard ports use the
			// audio_url override above.
			return (&url.URL{Scheme: "isapi", User: u.User, Host: u.Hostname()}).String()
		}
	}
	return ""
}

// wireTwoWayAudioSinks attaches a derived ISAPI talk sink to every stream of
// each talk-enabled profile, so `two_way_audio: true` needs no second config
// line and no restated credentials. The sink is a lazy producer — inert until
// Nx actually presses talk and the RTSP backchannel attaches.
func wireTwoWayAudioSinks(profiles []onvif.OnvifProfile) {
	for _, profile := range profiles {
		if !profile.TwoWayAudio {
			continue
		}
		p := profile // capture loop variable for &p
		for _, s := range p.Streams {
			name, _, _, _, _, _, _ := onvif.ParseStream(s)
			sink := resolveCameraISAPIURL(&p, name)
			if sink == "" {
				log.Warn().Str("profile", p.Name).Str("stream", name).
					Msg("[onvif] two_way_audio: could not derive ISAPI sink from stream source (set audio_url?)")
				continue
			}
			st := streams.Get(name)
			if st == nil {
				continue
			}
			st.AddSource(sink)
			redacted := sink
			if u, err := url.Parse(sink); err == nil {
				redacted = u.Redacted()
			}
			log.Info().Str("stream", name).Str("sink", redacted).Msg("[onvif] two-way audio sink wired")
		}
	}
}

// profileByToken finds the profile owning a media profile token (a stream name).
func profileByToken(profiles []onvif.OnvifProfile, token string) *onvif.OnvifProfile {
	for i := range profiles {
		for _, s := range profiles[i].Streams {
			if name, _, _, _, _, _, _ := onvif.ParseStream(s); name == token {
				return &profiles[i]
			}
		}
	}
	return nil
}

// resolveCameraOnvifURL derives the camera's ONVIF endpoint for PTZ from the
// profile's stream source — no need to restate creds. Order: explicit ptz_url,
// then an onvif:// source, then derive from an rtsp/http source (same host +
// creds, default ONVIF http port/path).
func resolveCameraOnvifURL(p *onvif.OnvifProfile) string {
	if p == nil || !p.Ptz {
		return ""
	}
	if p.PtzURL != "" {
		return p.PtzURL
	}
	for _, s := range p.Streams {
		name, _, _, _, _, _, _ := onvif.ParseStream(s)
		st := streams.Get(name)
		if st == nil {
			continue
		}
		srcs := st.Sources()
		for _, src := range srcs {
			if strings.HasPrefix(src, "onvif://") {
				return src
			}
		}
		for _, src := range srcs {
			u, err := url.Parse(src)
			if err != nil || u.User == nil {
				continue
			}
			switch u.Scheme {
			case "rtsp", "rtsps", "http", "https":
				return (&url.URL{Scheme: "onvif", User: u.User, Host: u.Hostname()}).String()
			}
		}
	}
	return ""
}

func getPTZCamera(camURL string) (*ptzCamera, error) {
	ptzMu.Lock()
	defer ptzMu.Unlock()
	if pc := ptzCache[camURL]; pc != nil {
		return pc, nil
	}
	client, err := onvif.NewClient(camURL)
	if err != nil {
		return nil, err
	}
	if !client.HasPTZ() {
		return nil, errors.New("camera advertises no PTZ service")
	}
	tokens, err := client.GetProfilesTokens()
	if err != nil || len(tokens) == 0 {
		return nil, errors.New("camera has no media profiles for PTZ")
	}
	pc := &ptzCamera{client: client, token: tokens[0]}
	ptzCache[camURL] = pc
	log.Info().Str("camera", camURL).Str("token", pc.token).Msg("[onvif] PTZ relay client ready")
	return pc, nil
}

// relayPTZ forwards a movement/preset operation to the backend camera.
func relayPTZ(operation string, b []byte, profiles []onvif.OnvifProfile) ([]byte, error) {
	token := onvif.FindTagValue(b, "ProfileToken")
	p := profileByToken(profiles, token)
	camURL := resolveCameraOnvifURL(p)
	if camURL == "" {
		return nil, errors.New("ptz not configured for profile " + token)
	}
	cam, err := getPTZCamera(camURL)
	if err != nil {
		return nil, err
	}
	switch operation {
	case onvif.PTZContinuousMove:
		x, y, z := onvif.ParsePTZVelocity(b)
		return cam.client.PTZContinuousMove(cam.token, x, y, z)
	case onvif.PTZStop:
		return cam.client.PTZStop(cam.token)
	case onvif.PTZGetPresets:
		return cam.client.PTZGetPresets(cam.token)
	case onvif.PTZGotoPreset:
		return cam.client.PTZGotoPreset(cam.token, onvif.FindTagValue(b, "PresetToken"))
	}
	return nil, errors.New("unsupported ptz operation " + operation)
}

// startCameraServer starts a standalone HTTP server for a single ONVIF camera profile.
// ip may be nil to listen on all interfaces, or a specific IP for virtual-IP setups.
func startCameraServer(ip net.IP, port int, uuid string, handler http.HandlerFunc) {
	var addr string
	if ip != nil {
		addr = ip.String() + ":" + strconv.Itoa(port)
	} else {
		addr = ":" + strconv.Itoa(port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error().Err(err).Msgf("[onvif] camera server failed to start on %s", addr)
		return
	}
	log.Info().Msgf("[onvif] camera server listening on %s (uuid=%s)", addr, uuid)

	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/", handler)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.Serve(ln); err != nil {
		log.Error().Err(err).Msgf("[onvif] camera server error on port %d", port)
	}
}

// makeOnvifHandler returns an ONVIF device service handler scoped to the given
// profiles. mainAPIPort is the go2rtc API port used for snapshot URLs (the
// snapshot endpoint always lives on the main server, not per-camera servers).
// deviceName is advertised in GetScopes and GetDeviceInformation responses.
func makeOnvifHandler(profiles []onvif.OnvifProfile, mainAPIPort int, deviceName string) http.HandlerFunc {
	// Local camera-name lookup scoped to this handler's profiles.
	cameraName := func(streamName string) string {
		for _, p := range profiles {
			for _, s := range p.Streams {
				name, _, _, _, _, _, _ := onvif.ParseStream(s)
				if name == streamName {
					return p.Name
				}
			}
		}
		return "Unknown Camera"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		operation := onvif.GetRequestAction(b)
		if operation == "" {
			http.Error(w, "malformed request body", http.StatusBadRequest)
			return
		}

		log.Trace().Msgf("[onvif] server request %s %s:\n%s", r.Method, r.RequestURI, b)

		// WS-Security auth on incoming requests. Required when ANY profile
		// served by this handler sets a non-empty Password (per-profile
		// credentials in YAML). The request is accepted if it matches at
		// least one such profile's credentials — this matters for the
		// shared :1984 handler that holds all profiles; per-camera handlers
		// have a single profile so any-vs-all is moot there.
		//
		// GetSystemDateAndTime is exempt — clients must call it before they
		// can compute a PasswordDigest (Nx queries device time first to
		// align Created within the device's clock window).
		if operation != onvif.DeviceGetSystemDateAndTime {
			authRequired := false
			authMatched := false
			for _, p := range profiles {
				if p.Password == "" {
					continue
				}
				authRequired = true
				if onvif.ValidateUsernameToken(b, p.Username, p.Password) {
					authMatched = true
					break
				}
			}
			if authRequired && !authMatched {
				log.Debug().Str("op", operation).Msg("[onvif] auth failed")
				w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(onvif.AuthFaultEnvelope())
				return
			}
		}

		switch operation {
		case onvif.DeviceGetSystemDateAndTime, // important for Hass
			onvif.DeviceGetDiscoveryMode,
			onvif.DeviceGetDNS,
			onvif.DeviceGetHostname,
			onvif.DeviceGetNetworkDefaultGateway,
			onvif.DeviceGetNetworkProtocols,
			onvif.DeviceGetNTP:
			b = onvif.StaticResponse(operation)

		case onvif.DeviceGetNetworkInterfaces:
			// Nx Witness keys cameras by MAC address from this response
			// and aborts the add flow if it's empty — synthesise a
			// realistic NetworkInterfaces entry with stable per-profile
			// MAC (derived from profile name unless device_info.mac
			// overrides) and the host IP from the request.
			host, _, splitErr := net.SplitHostPort(r.Host)
			if splitErr != nil {
				host = r.Host
			}
			b = onvif.GetNetworkInterfacesResponse(profiles, host)

		case onvif.DeviceGetScopes:
			b = onvif.GetScopesResponse(deviceName, anyTwoWayAudio(profiles))

		case onvif.MediaGetVideoEncoderConfigurations:
			b = onvif.GetVideoEncoderConfigurationsResponse(profiles)

		case onvif.MediaGetAudioSources:
			b = onvif.GetAudioSourcesResponse(profiles)

		case onvif.MediaGetAudioSourceConfigurations:
			b = onvif.GetAudioSourceConfigurationsResponse(profiles)

		case onvif.MediaGetAudioEncoderConfigurations:
			b = onvif.GetAudioEncoderConfigurationsResponse(profiles)

		case onvif.MediaGetAudioEncoderConfigurationOptions:
			// Nx Witness calls this when the operator ticks "Enable audio".
			// Returning "operation not supported" (the upstream default for
			// unhandled SOAP) makes Nx reject audio with "audio is not
			// configured properly" — same shape as the video options bug.
			token := onvif.FindTagValue(b, "ConfigurationToken")
			b = onvif.GetAudioEncoderConfigurationOptionsResponse(profiles, token)

		// ----- audio OUTPUT (two-way audio): un-greys Nx's talk button -----
		case onvif.MediaGetAudioOutputs:
			b = onvif.GetAudioOutputsResponse(profiles)

		case onvif.MediaGetAudioOutputConfigurations:
			b = onvif.GetAudioOutputConfigurationsResponse(profiles)

		case onvif.MediaGetCompatibleAudioOutputConfigurations:
			b = onvif.GetCompatibleAudioOutputConfigurationsResponse(profiles)

		case onvif.MediaGetAudioOutputConfiguration:
			name := strings.TrimSuffix(onvif.FindTagValue(b, "ConfigurationToken"), "_aoutcfg")
			b = onvif.GetAudioOutputConfigurationResponse(name)

		case onvif.MediaGetAudioOutputConfigurationOptions:
			b = onvif.GetAudioOutputConfigurationOptionsResponse(profiles)

		case onvif.MediaGetAudioDecoderConfigurations:
			b = onvif.GetAudioDecoderConfigurationsResponse(profiles)

		case onvif.MediaGetCompatibleAudioDecoderConfigurations:
			b = onvif.GetCompatibleAudioDecoderConfigurationsResponse(profiles)

		case onvif.MediaGetAudioDecoderConfigurationOptions:
			b = onvif.GetAudioDecoderConfigurationOptionsResponse()

		case onvif.DeviceGetCapabilities:
			// important for Hass: Media section
			b = onvif.GetCapabilitiesResponse(r.Host, anyPTZ(profiles))

		case onvif.DeviceGetServices:
			b = onvif.GetServicesResponse(r.Host, anyPTZ(profiles))

		case onvif.DeviceGetOSDs:
			token := onvif.FindTagValue(b, "ConfigurationToken")
			b = onvif.GetOSDsResponse(token, cameraName(onvif.StreamNameFromConfigToken(token)))

		case onvif.DeviceGetOSDOptions:
			b = onvif.GetOSDOptionsResponse()

		case onvif.DeviceGetDeviceInformation:
			// Defaults preserve upstream behaviour:
			//   Manufacturer = camera name (UP shows "<cameraName> go2rtc")
			//   Model        = "go2rtc"
			//   Serial       = r.Host (includes port; unique per-camera server)
			//   HardwareId   = "1.00"
			// Per-profile device_info overrides drive VMS driver matching
			// (e.g. Nx Witness allocating an encoder license requires
			// Manufacturer + Model strings on its encoder driver list).
			manuf := deviceName
			model := "go2rtc"
			firmware := app.Version
			serial := r.Host
			hardwareId := "1.00"
			if len(profiles) == 1 {
				di := profiles[0].DeviceInfo
				if di.Manufacturer != "" {
					manuf = di.Manufacturer
				}
				if di.Model != "" {
					model = di.Model
				}
				if di.FirmwareVersion != "" {
					firmware = di.FirmwareVersion
				}
				if di.SerialNumber != "" {
					serial = di.SerialNumber
				}
				if di.HardwareId != "" {
					hardwareId = di.HardwareId
				}
			}
			b = onvif.GetDeviceInformationResponse(manuf, model, firmware, serial, hardwareId)

		case onvif.ServiceGetServiceCapabilities:
			// important for Hass
			b = onvif.GetMediaServiceCapabilitiesResponse()

		case onvif.DeviceSystemReboot:
			b = onvif.StaticResponse(operation)

			time.AfterFunc(time.Second, func() {
				os.Exit(0)
			})

		case onvif.MediaGetVideoSources:
			b = onvif.GetVideoSourcesResponse(profiles)

		case onvif.MediaGetProfiles:
			// important for Hass: H264 codec, width, height
			b = onvif.GetProfilesResponse(profiles)

		case onvif.MediaGetProfile:
			token := onvif.FindTagValue(b, "ProfileToken")
			for _, profile := range profiles {
				for _, stream := range profile.Streams {
					name, _, _, _, _, _, _ := onvif.ParseStream(stream)
					if name == token {
						b = onvif.GetProfileResponse(profile)
						break
					}
				}
			}

		case onvif.MediaGetVideoSourceConfigurations:
			// important for Happytime Onvif Client
			b = onvif.GetVideoSourceConfigurationsResponse(profiles)

		case onvif.MediaGetVideoSourceConfiguration:
			token := onvif.FindTagValue(b, "ConfigurationToken")
			b = onvif.GetVideoSourceConfigurationResponse(token, profiles)

		case onvif.MediaGetVideoSourceConfigurationOptions:
			// Nx Witness device-add validation; without this Nx loops the probe
			token := onvif.FindTagValue(b, "ConfigurationToken")
			b = onvif.GetVideoSourceConfigurationOptionsResponse(profiles, token)

		case onvif.MediaGetCompatibleVideoEncoderConfigurations:
			// Nx Witness device-add validation; without this Nx loops the probe
			token := onvif.FindTagValue(b, "ProfileToken")
			b = onvif.GetCompatibleVideoEncoderConfigurationsResponse(profiles, token)

		case onvif.MediaGetVideoEncoderConfigurationOptions:
			// Nx Witness fails the device-add with "unknown_error" if this
			// returns "operation not supported". Pinned ranges matching
			// the actual encoder config.
			token := onvif.FindTagValue(b, "ConfigurationToken")
			b = onvif.GetVideoEncoderConfigurationOptionsResponse(profiles, token)

		case onvif.MediaGetStreamUri:
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			uri := "rtsp://" + host + ":" + rtsp.Port + "/" + onvif.FindTagValue(b, "ProfileToken")
			log.Debug().Msgf("[onvif] MediaGetStreamUri URL: %s", uri)
			b = onvif.GetStreamUriResponse(uri)

		case onvif.MediaGetSnapshotUri:
			// Snapshot is always served by the main go2rtc API, not the per-camera
			// server, so use mainAPIPort rather than whatever port this request arrived on.
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			uri := "http://" + host + ":" + strconv.Itoa(mainAPIPort) + "/api/frame.jpeg?src=" + onvif.FindTagValue(b, "ProfileToken")
			log.Debug().Msgf("[onvif] Snapshot URL: %s", uri)
			b = onvif.GetSnapshotUriResponse(uri)

		// ----- PTZ service: synthesised metadata (go2rtc tokens) -----
		case onvif.PTZGetNodes:
			b = onvif.GetPTZNodesResponse()
		case onvif.PTZGetNode:
			b = onvif.GetPTZNodeResponse()
		case onvif.PTZGetConfigurations:
			b = onvif.GetPTZConfigurationsResponse(profiles)
		case onvif.PTZGetConfiguration:
			name := strings.TrimPrefix(onvif.FindTagValue(b, "PTZConfigurationToken"), "ptzcfg_")
			b = onvif.GetPTZConfigurationResponse(name)
		case onvif.PTZGetConfigurationOptions:
			b = onvif.GetPTZConfigurationOptionsResponse()
		case onvif.PTZGetStatus:
			b = onvif.GetPTZStatusResponse()

		// ----- PTZ service: motion/presets relayed to the backend camera -----
		case onvif.PTZContinuousMove, onvif.PTZStop, onvif.PTZGetPresets, onvif.PTZGotoPreset:
			resp, ptzErr := relayPTZ(operation, b, profiles)
			if ptzErr != nil {
				log.Warn().Err(ptzErr).Str("op", operation).Msg("[onvif] ptz relay failed")
				http.Error(w, ptzErr.Error(), http.StatusBadGateway)
				return
			}
			b = resp

		default:
			http.Error(w, "unsupported operation", http.StatusBadRequest)
			log.Warn().Msgf("[onvif] unsupported operation: %s", operation)
			log.Debug().Msgf("[onvif] unsupported request:\n%s", b)
			return
		}

		log.Trace().Msgf("[onvif] server response:\n%s", b)

		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		if _, err = w.Write(b); err != nil {
			log.Error().Err(err).Caller().Send()
		}
	}
}

// GetConfiguredStreams returns the stream names that should be accessible via ONVIF.
func GetConfiguredStreams() []string {
	if len(OnvifProfiles) == 0 {
		return streams.GetAllNames()
	}

	var names []string
	for _, profile := range OnvifProfiles {
		for _, stream := range profile.Streams {
			name, _, _, _, _, _, _ := onvif.ParseStream(stream)
			names = append(names, name)
		}
	}
	return names
}

func streamOnvif(rawURL string) (core.Producer, error) {
	client, err := onvif.NewClient(rawURL)
	if err != nil {
		return nil, err
	}

	uri, err := client.GetURI()
	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("[onvif] new uri=%s", uri)

	return streams.GetProducer(uri)
}

func apiOnvif(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")

	var items []*api.Source

	if src == "" {
		urls, err := onvif.DiscoveryStreamingURLs()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, rawURL := range urls {
			u, err := url.Parse(rawURL)
			if err != nil {
				log.Warn().Str("url", rawURL).Msg("[onvif] broken")
				continue
			}

			if u.Scheme != "http" {
				log.Warn().Str("url", rawURL).Msg("[onvif] unsupported")
				continue
			}

			u.Scheme = "onvif"
			u.User = url.UserPassword("user", "pass")

			if u.Path == onvif.PathDevice {
				u.Path = ""
			}

			items = append(items, &api.Source{Name: u.Host, URL: u.String()})
		}
	} else {
		client, err := onvif.NewClient(src)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if l := log.Trace(); l.Enabled() {
			b, _ := client.MediaRequest(onvif.MediaGetProfiles)
			l.Msgf("[onvif] src=%s profiles:\n%s", src, b)
		}

		name, err := client.GetName()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		tokens, err := client.GetProfilesTokens()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for i, token := range tokens {
			items = append(items, &api.Source{
				Name: name + " stream" + strconv.Itoa(i),
				URL:  src + "?subtype=" + token,
			})
		}

		if len(tokens) > 0 && client.HasSnapshots() {
			items = append(items, &api.Source{
				Name: name + " snapshot",
				URL:  src + "?subtype=" + tokens[0] + "&snapshot",
			})
		}
	}

	api.ResponseSources(w, items)
}
