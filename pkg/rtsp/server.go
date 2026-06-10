package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
)

var FailedAuth = errors.New("failed authentication")

func NewServer(conn net.Conn) *Conn {
	return &Conn{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "rtsp",
			Protocol:   "rtsp+tcp",
			RemoteAddr: conn.RemoteAddr().String(),
		},
		conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

func (c *Conn) Auth(username, password string) {
	info := url.UserPassword(username, password)
	c.auth = tcp.NewAuth(info)
}

func (c *Conn) Accept() error {
	for {
		req, err := c.ReadRequest()
		if err != nil {
			return err
		}

		if c.URL == nil {
			c.URL = req.URL
			c.UserAgent = req.Header.Get("User-Agent")
		}

		c.Fire(req)

		if valid, empty := c.auth.Validate(req); !valid {
			res := &tcp.Response{
				Status:  "401 Unauthorized",
				Header:  map[string][]string{"Www-Authenticate": {`Basic realm="go2rtc"`}},
				Request: req,
			}
			if err = c.WriteResponse(res); err != nil {
				return err
			}
			if empty {
				// eliminate false positive: ffmpeg sends first request without
				// authorization header even if the user provides credentials
				continue
			}
			return FailedAuth
		}

		// Receiver: OPTIONS > DESCRIBE > SETUP... > PLAY > TEARDOWN
		// Sender: OPTIONS > ANNOUNCE > SETUP... > RECORD > TEARDOWN
		switch req.Method {
		case MethodOptions:
			res := &tcp.Response{
				Header: map[string][]string{
					"Public": {"OPTIONS, SETUP, TEARDOWN, DESCRIBE, PLAY, PAUSE, ANNOUNCE, RECORD"},
				},
				Request: req,
			}
			if err = c.WriteResponse(res); err != nil {
				return err
			}

		case MethodAnnounce:
			if req.Header.Get("Content-Type") != "application/sdp" {
				return errors.New("wrong content type")
			}

			c.SDP = string(req.Body) // for info

			c.Medias, err = UnmarshalSDP(req.Body)
			if err != nil {
				return err
			}

			// TODO: fix someday...
			for i, media := range c.Medias {
				track := core.NewReceiver(media, media.Codecs[0])
				track.ID = byte(i * 2)
				c.Receivers = append(c.Receivers, track)
			}

			c.mode = core.ModePassiveProducer
			c.Fire(MethodAnnounce)

			res := &tcp.Response{Request: req}
			if err = c.WriteResponse(res); err != nil {
				return err
			}

		case MethodDescribe:
			c.mode = core.ModePassiveConsumer
			// ONVIF clients (e.g. Nx Witness) request a two-way-audio talk
			// channel via the standard backchannel Require header rather than
			// go2rtc's proprietary ?backchannel=1 query param. Surface it on the
			// Conn so the stream handler advertises a sendonly audio track in the
			// DESCRIBE SDP. Reusing the existing Backchannel field is safe here:
			// it is only read on the client (ModeActiveProducer) path, which a
			// server Conn never takes.
			if hasBackchannelRequire(req) {
				c.Backchannel = true
			}
			c.Fire(MethodDescribe)

			if c.Senders == nil {
				res := &tcp.Response{
					Status:  "404 Not Found",
					Request: req,
				}
				return c.WriteResponse(res)
			}

			res := &tcp.Response{
				Header: map[string][]string{
					"Content-Type": {"application/sdp"},
				},
				Request: req,
			}

			// convert tracks to real output medias medias
			var medias []*core.Media
			for i, track := range c.Senders {
				media := &core.Media{
					Kind:      core.GetKind(track.Codec.Name),
					Direction: core.DirectionRecvonly,
					Codecs:    []*core.Codec{track.Codec},
					ID:        "trackID=" + strconv.Itoa(i),
				}
				medias = append(medias, media)
			}

			for i, track := range c.Receivers {
				media := &core.Media{
					Kind:      core.GetKind(track.Codec.Name),
					Direction: core.DirectionSendonly,
					Codecs:    []*core.Codec{track.Codec},
					ID:        "trackID=" + strconv.Itoa(i+len(c.Senders)),
				}
				medias = append(medias, media)
			}

			res.Body, err = core.MarshalSDP(c.SessionName, medias)
			if err != nil {
				return err
			}

			c.SDP = string(res.Body) // for info

			if err = c.WriteResponse(res); err != nil {
				return err
			}

		case MethodSetup:
			res := &tcp.Response{
				Header:  map[string][]string{},
				Request: req,
			}

			// Test if client requests TCP transport, otherwise return 461 Transport not supported
			// This allows smart clients who initially requested UDP to fall back on TCP transport
			if tr := req.Header.Get("Transport"); strings.HasPrefix(tr, "RTP/AVP/TCP") {
				c.session = core.RandString(8, 10)
				c.state = StateSetup

				if c.mode == core.ModePassiveConsumer {
					if i := reqTrackID(req); i >= 0 && i < len(c.Senders)+len(c.Receivers) {
						if i < len(c.Senders) {
							c.Senders[i].Media.ID = MethodSetup
						} else {
							c.Receivers[i-len(c.Senders)].Media.ID = MethodSetup
						}
						tr = fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", i*2, i*2+1)
						res.Header.Set("Transport", tr)
					} else {
						res.Status = "400 Bad Request"
					}
				} else {
					res.Header.Set("Transport", tr)
				}
			} else {
				res.Status = "461 Unsupported transport"
			}

			if err = c.WriteResponse(res); err != nil {
				return err
			}

		case MethodRecord, MethodPlay:
			if c.mode == core.ModePassiveConsumer {
				// stop unconfigured senders
				for _, track := range c.Senders {
					if track.Media.ID != MethodSetup {
						track.Close()
					}
				}
			}

			res := &tcp.Response{
				Header:  map[string][]string{},
				Request: req,
			}

			// RTP-Info header — required by strict RTSP clients (notably
			// Nx Witness's Dahua driver) to start RTP stream
			// synchronisation. Without it the client accepts the PLAY
			// 200 OK and then immediately TEARDOWNs (observed during
			// 2026-05-09 pilot — VLC tolerates the missing header,
			// Nx does not).
			if c.mode == core.ModePassiveConsumer && req.Method == MethodPlay {
				baseURL := req.URL.String()
				var parts []string
				for i, sender := range c.Senders {
					if sender.Media.ID != MethodSetup {
						continue
					}
					parts = append(parts, fmt.Sprintf("url=%s/trackID=%d;seq=1;rtptime=0", baseURL, i))
				}
				if len(parts) > 0 {
					res.Header.Set("RTP-Info", strings.Join(parts, ","))
				}
			}

			err = c.WriteResponse(res)
			c.playOK = true
			return err

		case MethodTeardown:
			res := &tcp.Response{Request: req}
			_ = c.WriteResponse(res)
			c.state = StateNone
			return c.conn.Close()

		default:
			return fmt.Errorf("unsupported method: %s", req.Method)
		}
	}
}

// hasBackchannelRequire reports whether an RTSP DESCRIBE asks for the ONVIF
// audio backchannel (token www.onvif.org/ver20/backchannel) via the Require
// header. Matched as a case-insensitive substring so the trailing ": true"
// value form some clients send is tolerated.
func hasBackchannelRequire(req *tcp.Request) bool {
	return strings.Contains(strings.ToLower(req.Header.Get("Require")), "backchannel")
}

func reqTrackID(req *tcp.Request) int {
	var s string
	if req.URL.RawQuery != "" {
		s = req.URL.RawQuery
	} else {
		s = req.URL.Path
	}
	if i := strings.LastIndexByte(s, '='); i > 0 {
		if i, err := strconv.Atoi(s[i+1:]); err == nil {
			return i
		}
	}
	return -1
}
