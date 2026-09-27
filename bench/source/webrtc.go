package main

import (
	"context"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// rtcOrigin forwards the encoder's RTP (H.264 + Opus, from ffmpeg's tee)
// to every connected browser over WebRTC — an SFU with one publisher.
// Packets are relayed as-is: no transcoding, no added buffering, so what
// the browser measures is WebRTC transport + its jitter buffer.
//
// What it can't do is what a production WebRTC sender does on a Picture
// Loss Indication from a new viewer: ask the encoder for a keyframe. The
// encoder here is shared with every other system and keeps a fixed 2s
// GOP, so a joining WebRTC viewer waits for the next scheduled keyframe.
type rtcOrigin struct {
	api   *webrtc.API
	video *webrtc.TrackLocalStaticRTP
	audio *webrtc.TrackLocalStaticRTP

	mu        sync.Mutex
	keyRTPTs  []uint32 // RTP timestamp of each keyframe, in order
	lastKeyTs uint32
	haveKey   bool

	// Encoder output clock. RTP leaves ffmpeg the moment a frame is
	// encoded (no muxing, no segmenting), so each frame's arrival minus
	// its timestamp, (arrival wall ms − (ts−ts0)/90), is when the encoder
	// emits frames. Every frame is kept, not just the minimum: where the
	// looped source restarts, ffmpeg's real-time pacing lets a few frames
	// out early, and a plain minimum locks onto those (a ~300ms error in
	// the first full run). The analysis takes a robust low percentile.
	ts0, lastTs uint32
	haveTs0     bool
	relMs       []int32   // frame timestamp, ms after the first frame
	offMs       []float32 // arrival − relMs, wall ms relative to offBase
	offBase     float64
}

type encoderClock struct {
	TS0     uint32    `json:"ts0"`
	OffBase float64   `json:"off_base_ms"`
	RelMs   []int32   `json:"rel_ms"`
	OffMs   []float32 `json:"off_ms"`
}

func (o *rtcOrigin) clock() *encoderClock {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.haveTs0 {
		return nil
	}
	return &encoderClock{TS0: o.ts0, OffBase: o.offBase,
		RelMs: append([]int32(nil), o.relMs...), OffMs: append([]float32(nil), o.offMs...)}
}

func newRTCOrigin() (*rtcOrigin, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	// Default interceptors: NACK retransmission, RTCP sender/receiver
	// reports (Chrome needs sender reports to sync audio with video), TWCC.
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se), webrtc.WithInterceptorRegistry(reg))

	// x264 with -bf 0 at 720p30: High profile, level 3.1.
	video, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f",
	}, "video", "bench")
	if err != nil {
		return nil, err
	}
	audio, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "audio", "bench")
	if err != nil {
		return nil, err
	}
	return &rtcOrigin{api: api, video: video, audio: audio}, nil
}

// relay reads RTP datagrams on addr and writes them to track until ctx ends.
func (o *rtcOrigin) relay(ctx context.Context, addr string, track *webrtc.TrackLocalStaticRTP, isVideo bool) error {
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return err
	}
	if u, ok := pc.(*net.UDPConn); ok {
		_ = u.SetReadBuffer(4 << 20)
	}
	go func() { <-ctx.Done(); pc.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p rtp.Packet
			if err := p.Unmarshal(buf[:n]); err != nil {
				continue
			}
			if isVideo {
				at := float64(time.Now().UnixMicro()) / 1000
				o.mu.Lock()
				if !o.haveTs0 {
					o.ts0, o.lastTs, o.haveTs0, o.offBase = p.Timestamp, p.Timestamp-1, true, math.Floor(at)
				}
				if p.Timestamp != o.lastTs { // first packet of a new frame
					o.lastTs = p.Timestamp
					rel := float64(p.Timestamp-o.ts0) / 90
					o.relMs = append(o.relMs, int32(rel))
					o.offMs = append(o.offMs, float32(math.Round((at-rel-o.offBase)*10)/10))
				}
				if startsKeyframe(p.Payload) && (!o.haveKey || p.Timestamp != o.lastKeyTs) {
					o.keyRTPTs = append(o.keyRTPTs, p.Timestamp)
					o.lastKeyTs, o.haveKey = p.Timestamp, true
				}
				o.mu.Unlock()
			}
			if err := track.WriteRTP(&p); err != nil && err != io.ErrClosedPipe {
				log.Printf("rtp write: %v", err)
			}
		}
	}()
	return nil
}

// startsKeyframe reports whether an H.264 RTP payload (RFC 6184) carries
// the start of an IDR access unit: an SPS or IDR NAL, alone, aggregated
// (STAP-A) or as the first fragment of a FU-A.
func startsKeyframe(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	isKey := func(t byte) bool { return t == 5 || t == 7 }
	switch t := b[0] & 0x1f; t {
	case 24: // STAP-A: [2-byte size][NAL]...
		for i := 1; i+2 < len(b); {
			size := int(b[i])<<8 | int(b[i+1])
			if i+2 < len(b) && isKey(b[i+2]&0x1f) {
				return true
			}
			i += 2 + size
		}
		return false
	case 28: // FU-A: start bit + original type in the FU header
		return b[1]&0x80 != 0 && isKey(b[1]&0x1f)
	default:
		return isKey(t)
	}
}

func (o *rtcOrigin) keyframes() []uint32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]uint32(nil), o.keyRTPTs...)
}

// ServeHTTP is WHEP-style signaling: POST an SDP offer, get the answer
// with all of the server's candidates (no trickle; everything is local).
func (o *rtcOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST an SDP offer", http.StatusMethodNotAllowed)
		return
	}
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pc, err := o.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fail := func(err error) {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	for _, t := range []*webrtc.TrackLocalStaticRTP{o.video, o.audio} {
		sender, err := pc.AddTrack(t)
		if err != nil {
			fail(err)
			return
		}
		go func() { // drain RTCP (receiver reports, PLIs) so the sender never blocks
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected || s == webrtc.PeerConnectionStateClosed {
			pc.Close()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		fail(err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		fail(err)
		return
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		fail(err)
		return
	}
	<-gathered
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(pc.LocalDescription().SDP))
}
