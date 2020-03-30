package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"filippo.io/cpace"
	"github.com/pion/webrtc/v2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/secretbox"
)

// Accessing pion/webrtc APIs like DataChannel.Detach() requires
// that we do this voodoo.
var rtcapi *webrtc.API

func init() {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	rtcapi = webrtc.NewAPI(webrtc.WithSettingEngine(s))
}

// conn is a wrapper around webrtc.DataChannel.
type conn struct {
	io.ReadWriteCloser
	d  *webrtc.DataChannel
	pc *webrtc.PeerConnection

	slot    string
	sigserv string
	etag    string

	// opened signals that the underlying DataChannel is open and ready
	// to handle data.
	opened chan struct{}
	// err forwards errors from the OnError callback.
	err chan error
	// flushc is a condition variable to coordinate flushed state of the
	// underlying channel.
	flushc *sync.Cond
}

func (c *conn) Write(p []byte) (n int, err error) {
	// The webrtc package's channel does not have a blocking Write, so
	// we can't just use io.Copy until the issue is fixed upsteam.
	// Work around this by blocking here and waiting for flushes.
	// https://github.com/pion/sctp/issues/77
	c.flushc.L.Lock()
	for c.d.BufferedAmount() > c.d.BufferedAmountLowThreshold() {
		c.flushc.Wait()
	}
	c.flushc.L.Unlock()
	return c.ReadWriteCloser.Write(p)
}

// TODO benchmark this buffer madness.
func (c *conn) flushed() {
	c.flushc.L.Lock()
	c.flushc.Signal()
	c.flushc.L.Unlock()
}

func (c *conn) Close() (err error) {
	for c.d.BufferedAmount() != 0 {
		// SetBufferedAmountLowThreshold does not seem to take effect
		// when after the last Write().
		time.Sleep(time.Second) // ew.
	}
	tryclose := func(c io.Closer) {
		e := c.Close()
		if e != nil {
			err = e
		}
	}
	defer tryclose(c.pc)
	defer tryclose(c.d)
	defer tryclose(c.ReadWriteCloser)
	return nil
}

func (c *conn) open() {
	var err error
	c.ReadWriteCloser, err = c.d.Detach()
	if err != nil {
		log.Printf("could not detatch data channel: %v", err)
	}
	close(c.opened)
}

// It's not really clear to me when this will be invoked.
func (c *conn) error(err error) {
	log.Printf("debug: %v", err)
	c.err <- err
}

// exchange is the container used to send data to signalling server
type exchange struct {
	Msg    string `json:"msg"`
	Secret string `json:"secret"`
}

func (c *conn) a(pass string) error {
	// The identity arguments are to bind endpoint identities in PAKE. Cf. Unknown
	// Key-Share Attack. https://tools.ietf.org/html/draft-ietf-mmusic-sdp-uks-03
	//
	// In the context of a program like magic-wormhole we do not have ahead of time
	// information on the identity of the remote party. We only have the slot name.
	// But that's okay, since:
	//   a) The password is randomly generated and ephemeral.
	//   b) A peer only gets one guess.
	// An unintended destination is likely going to fail PAKE.
	msgA, pake, err := cpace.Start(pass, cpace.NewContextInfo("", "", []byte(c.slot)))
	log.Printf("sending msg a")
	resp, status, err := c.put(exchange{
		Msg: base64.URLEncoding.EncodeToString(msgA),
	})
	if err != nil {
		return err
	}

	if status == http.StatusPreconditionRequired {
		// We are actually B.
		return c.b(pass, resp)
	}
	if status != http.StatusOK {
		return errors.New("a: bad status code")
	}

	msgB, err := base64.URLEncoding.DecodeString(resp.Msg)
	if err != nil {
		return err
	}
	mk, err := pake.Finish(msgB)
	hkdf := hkdf.New(sha256.New, mk, nil, nil)
	k := [32]byte{}
	_, err = io.ReadFull(hkdf, k[:])
	if err != nil {
		return err
	}

	soffer, err := base64.URLEncoding.DecodeString(resp.Secret)
	if err != nil {
		return err
	}
	var nonce [24]byte
	copy(nonce[:], soffer[:24])
	jsonoffer, ok := secretbox.Open(nil, soffer[24:], &nonce, &k)
	if !ok {
		// Bad key. Send an answer anyway so the other side knows.
		if _, err := io.ReadFull(crand.Reader, nonce[:]); err != nil {
			return err
		}
		c.del(exchange{
			Secret: base64.URLEncoding.EncodeToString(
				secretbox.Seal(nonce[:], []byte("bad key"), &nonce, &k),
			),
		})
		return errors.New("bad key")
	}
	var offer webrtc.SessionDescription
	err = json.Unmarshal(jsonoffer, &offer)
	if err != nil {
		return err
	}
	err = c.pc.SetRemoteDescription(offer)
	if err != nil {
		return err
	}
	answer, err := c.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	err = c.pc.SetLocalDescription(answer)
	if err != nil {
		return err
	}
	jsonanswer, err := json.Marshal(answer)
	if err != nil {
		return err
	}

	log.Printf("sending answer")
	if _, err := io.ReadFull(crand.Reader, nonce[:]); err != nil {
		return err
	}
	return c.del(exchange{
		Secret: base64.URLEncoding.EncodeToString(
			secretbox.Seal(nonce[:], jsonanswer, &nonce, &k),
		),
	})
}

func (c *conn) b(pass string, resp exchange) error {
	msgA, err := base64.URLEncoding.DecodeString(resp.Msg)
	if err != nil {
		return err
	}
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	err = c.pc.SetLocalDescription(offer)
	if err != nil {
		return err
	}
	jsonoffer, err := json.Marshal(offer)
	if err != nil {
		return err
	}

	msgB, mk, err := cpace.Exchange(pass, cpace.NewContextInfo("", "", []byte(c.slot)), msgA)
	hkdf := hkdf.New(sha256.New, mk, nil, nil)
	k := [32]byte{}
	_, err = io.ReadFull(hkdf, k[:])
	if err != nil {
		return err
	}
	var nonce [24]byte
	if _, err := io.ReadFull(crand.Reader, nonce[:]); err != nil {
		return err
	}
	log.Printf("sending msg b + offer")
	resp, status, err := c.put(exchange{
		Msg: base64.URLEncoding.EncodeToString(msgB),
		Secret: base64.URLEncoding.EncodeToString(
			secretbox.Seal(nonce[:], jsonoffer, &nonce, &k),
		),
	})
	if status != http.StatusOK {
		return errors.New("b: bad status code")
	}

	sanswer, err := base64.URLEncoding.DecodeString(resp.Secret)
	if err != nil {
		return err
	}
	copy(nonce[:], sanswer[:24])
	jsonanswer, ok := secretbox.Open(nil, sanswer[24:], &nonce, &k)
	if !ok {
		return errors.New("bad key")
	}
	var answer webrtc.SessionDescription
	err = json.Unmarshal(jsonanswer, &answer)
	if err != nil {
		return err
	}
	return c.pc.SetRemoteDescription(answer)
}

func (c *conn) del(e exchange) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, c.sigserv+c.slot, bytes.NewReader(body))
	if c.etag != "" {
		req.Header.Add("If-Match", c.etag)
	}
	req.Header.Add("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if r.StatusCode != http.StatusOK {
		return errors.New("bad status code")
	}
	return nil
}

func (c *conn) put(e exchange) (ans exchange, status int, err error) {
	body, err := json.Marshal(e)
	if err != nil {
		return ans, 0, err
	}
	req, err := http.NewRequest(http.MethodPut, c.sigserv+c.slot, bytes.NewReader(body))
	if c.etag != "" {
		req.Header.Add("If-Match", c.etag)
	}
	req.Header.Add("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return ans, 0, err
	}
	c.etag = r.Header.Get("ETag")
	err = json.NewDecoder(r.Body).Decode(&ans)
	return ans, r.StatusCode, err
}

// Dial returns an established WebRTC data channel to a peer.
//
// slot is used to synchronise with the remote peer on signalling server
// sigserv, and pass is used as the PAKE password authenticate the WebRTC
// offer and answer.
//
// iceserv is an optional list of STUN and TURN URLs to use for NAT traversal.
func Dial(slot, pass string, sigserv string, iceserv []string) (*conn, error) {
	c := &conn{
		slot:    slot,
		sigserv: sigserv,
		opened:  make(chan struct{}),
		err:     make(chan error),
		flushc:  sync.NewCond(&sync.Mutex{}),
	}

	rtccfg := webrtc.Configuration{}
	// TODO parse creds for turn servers
	for i := range iceserv {
		if iceserv[i] != "" {
			rtccfg.ICEServers = append(rtccfg.ICEServers, webrtc.ICEServer{URLs: []string{iceserv[i]}})
		}
	}
	var err error
	c.pc, err = rtcapi.NewPeerConnection(rtccfg)
	if err != nil {
		return nil, err
	}
	sigh := true
	c.d, err = c.pc.CreateDataChannel("data", &webrtc.DataChannelInit{
		Negotiated: &sigh,
		ID:         new(uint16),
	})
	if err != nil {
		return nil, err
	}
	c.d.OnOpen(c.open)
	c.d.OnError(c.error)
	c.d.OnBufferedAmountLow(c.flushed)
	// Any threshold amount >= 1MiB seems to occasionally lock up pion.
	// Choose 512 KiB as a safe default.
	// TODO look into why.
	c.d.SetBufferedAmountLowThreshold(512 << 10)

	// Start the handshake
	err = c.a(pass)
	if err != nil {
		return nil, err
	}

	select {
	case <-c.opened:
		return c, nil
	case err := <-c.err:
		return nil, err
	}

	return c, nil
}