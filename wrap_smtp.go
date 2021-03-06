package sparkypmtatracking

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"os"
	"strings"

	smtpproxy "github.com/tuck1s/go-smtpproxy"
)

//-----------------------------------------------------------------------------
// Backend handlers

// The Backend implements SMTP server methods.
type Backend struct {
	outHostPort        string
	verbose            bool
	upstreamDataDebug  *os.File
	wrapper            *Wrapper
	insecureSkipVerify bool
}

// NewBackend init function
func NewBackend(outHostPort string, verbose bool, upstreamDataDebug *os.File, wrap *Wrapper, insecureSkipVerify bool) *Backend {
	b := Backend{
		outHostPort:        outHostPort,
		verbose:            verbose,
		upstreamDataDebug:  upstreamDataDebug,
		wrapper:            wrap,
		insecureSkipVerify: insecureSkipVerify,
	}
	return &b
}

// SetVerbose allows changing logging options on-the-fly
func (bkd *Backend) SetVerbose(v bool) {
	bkd.verbose = v
}

// SetWrapper Verbose allows changing on-the-fly
func (bkd *Backend) SetWrapper(wrap *Wrapper) {
	bkd.wrapper = wrap
}

func (bkd *Backend) logger(args ...interface{}) {
	if bkd.verbose {
		log.Println(args...)
	}
}

func (bkd *Backend) loggerAlways(args ...interface{}) {
	log.Println(args...)
}

// MakeSession returns a session for this client and backend
func MakeSession(c *smtpproxy.Client, bkd *Backend) smtpproxy.Session {
	var s Session
	s.bkd = bkd    // just for logging
	s.upstream = c // keep record of the upstream Client connection
	return &s
}

// Init the backend. Here we establish the upstream connection
func (bkd *Backend) Init() (smtpproxy.Session, error) {
	bkd.logger("---Connecting upstream")
	c, err := smtpproxy.Dial(bkd.outHostPort)
	if err != nil {
		bkd.loggerAlways("< Connection error", bkd.outHostPort, err.Error())
		return nil, err
	}
	bkd.logger("< Connection success", bkd.outHostPort)
	return MakeSession(c, bkd), nil
}

//-----------------------------------------------------------------------------
// Session handlers

// A Session is returned after successful login. Here hold information that needs to persist across message phases.
type Session struct {
	bkd      *Backend          // The backend that created this session. Allows session methods to e.g. log
	upstream *smtpproxy.Client // the upstream client this backend is driving
}

// cmdTwiddle returns different flow markers depending on whether connection is secure (like Swaks does)
func cmdTwiddle(s *Session) string {
	if s.upstream != nil {
		if _, isTLS := s.upstream.TLSConnectionState(); isTLS {
			return "~>"
		}
	}
	return "->"
}

// respTwiddle returns different flow markers depending on whether connection is secure (like Swaks does)
func respTwiddle(s *Session) string {
	if s.upstream != nil {
		if _, isTLS := s.upstream.TLSConnectionState(); isTLS {
			return "\t<~"
		}
	}
	return "\t<-"
}

// Greet the upstream host and report capabilities back.
func (s *Session) Greet(helotype string) ([]string, int, string, error) {
	s.bkd.logger(cmdTwiddle(s), helotype)
	host, _, _ := net.SplitHostPort(s.bkd.outHostPort)
	if host == "" {
		host = "smtpproxy.localhost" // add dummy value in
	}
	code, msg, err := s.upstream.Hello(host)
	if err != nil {
		s.bkd.loggerAlways(respTwiddle(s), helotype, "error", err.Error())
		if code == 0 {
			// some errors don't show up in (code,msg) e.g. TLS cert errors, so map as a specific SMTP code/msg response
			code = 599
			msg = err.Error()
		}
		return nil, code, msg, err
	}
	s.bkd.logger(respTwiddle(s), helotype, "success")
	caps := s.upstream.Capabilities()
	s.bkd.logger("\tUpstream capabilities:", caps)
	return caps, code, msg, err
}

// StartTLS command
func (s *Session) StartTLS() (int, string, error) {
	host, _, _ := net.SplitHostPort(s.bkd.outHostPort)
	// Try the upstream server, it will report error if unsupported
	tlsconfig := &tls.Config{
		InsecureSkipVerify: s.bkd.insecureSkipVerify,
		ServerName:         host,
	}
	s.bkd.logger(cmdTwiddle(s), "STARTTLS")
	code, msg, err := s.upstream.StartTLS(tlsconfig)
	if err != nil {
		s.bkd.loggerAlways(respTwiddle(s), code, msg)
	} else {
		s.bkd.logger(respTwiddle(s), code, msg)
	}
	return code, msg, err
}

//Auth command backend handler
func (s *Session) Auth(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

//Mail command backend handler
func (s *Session) Mail(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

//Rcpt command backend handler
func (s *Session) Rcpt(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

//Reset command backend handler
func (s *Session) Reset(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

//Quit command backend handler
func (s *Session) Quit(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

//Unknown command backend handler
func (s *Session) Unknown(expectcode int, cmd, arg string) (int, string, error) {
	return s.Passthru(expectcode, cmd, arg)
}

// Passthru a command to the upstream server, logging
func (s *Session) Passthru(expectcode int, cmd, arg string) (int, string, error) {
	s.bkd.logger(cmdTwiddle(s), cmd, arg)
	joined := cmd
	if arg != "" {
		joined = cmd + " " + arg
	}
	code, msg, err := s.upstream.MyCmd(expectcode, joined)
	if err != nil {
		s.bkd.loggerAlways(respTwiddle(s), cmd, code, msg, "error", err.Error())
		if code == 0 {
			// some errors don't show up in (code,msg) e.g. TLS cert errors, so map as a specific SMTP code/msg response
			code = 599
			msg = err.Error()
		}
	} else {
		s.bkd.logger(respTwiddle(s), code, msg)
	}
	return code, msg, err
}

// DataCommand pass upstream, returning a place to write the data AND the usual responses
func (s *Session) DataCommand() (io.WriteCloser, int, string, error) {
	s.bkd.logger(cmdTwiddle(s), "DATA")
	w, code, msg, err := s.upstream.Data()
	if err != nil {
		s.bkd.loggerAlways(respTwiddle(s), "DATA error", err.Error())
	}
	return w, code, msg, err
}

// Data body (dot delimited) pass upstream, returning the usual responses
func (s *Session) Data(r io.Reader, w io.WriteCloser) (int, string, error) {
	var buf bytes.Buffer
	err := s.bkd.wrapper.MailCopy(&buf, r) // Pass in the engagement tracking info
	if err != nil {
		msg := "DATA MailCopy error"
		s.bkd.loggerAlways(respTwiddle(s), msg, err.Error())
		return 0, msg, err
	}
	// Upstream debug output - nondestructively read buf contents
	if s.bkd.upstreamDataDebug != nil {
		dbgWritten, err := io.Copy(s.bkd.upstreamDataDebug, bytes.NewReader(buf.Bytes()))
		if err != nil {
			s.bkd.loggerAlways("upstreamDataDebug error", err, ", bytes written =", dbgWritten)
			return 0, "", err
		}
	}
	// Send the data upstream
	count, err := io.Copy(w, &buf)
	if err != nil {
		msg := "DATA io.Copy error"
		s.bkd.loggerAlways(respTwiddle(s), msg, err.Error())
		return 0, msg, err
	}
	err = w.Close() // Need to close the data phase - then we should have response from upstream
	code := s.upstream.DataResponseCode
	msg := s.upstream.DataResponseMsg
	if err != nil {
		s.bkd.loggerAlways(respTwiddle(s), "DATA Close error", err, ", bytes written =", count)
		return 0, msg, err
	}
	if s.bkd.verbose {
		s.bkd.logger(respTwiddle(s), "DATA accepted, bytes written =", count)
	} else {
		// Short-form logging - one line per message - used when "verbose" not set
		log.Printf("Message DATA upstream,%d,%d,%s\n", count, code, msg)
	}
	return code, msg, err
}

//-----------------------------------------------------------------------------
// Engagement Tracking

// SparkPostMessageIDHeader is the email header name that carries the unique message ID
const SparkPostMessageIDHeader = "X-Sp-Message-Id"
const smtpCRLF = "\r\n"

// MailCopy transfers the mail body from downstream (client) to upstream (server), using the engagement wrapper
// The writer should be closed by the parent function
func (wrap *Wrapper) MailCopy(dst io.Writer, src io.Reader) error {
	if !wrap.Active() {
		_, err := io.Copy(dst, src) // wrapping inactive, just do a copy
		return err
	}
	message, err := mail.ReadMessage(src)
	if err != nil {
		return err
	}
	err = wrap.ProcessMessageHeaders(message.Header)
	if err != nil {
		return err
	}
	err = writeMessageHeaders(dst, message.Header)
	if err != nil {
		return err
	}
	// Handle the message body
	return wrap.HandleMessagePart(dst, message.Body, message.Header.Get("Content-Type"), message.Header.Get("Content-Transfer-Encoding"))
}

// ProcessMessageHeaders reads the message's current headers and updates/inserts any new ones required.
// The RCPT TO address is grabbed
func (wrap *Wrapper) ProcessMessageHeaders(h mail.Header) error {
	rcpts, err := h.AddressList("to")
	if err != nil {
		return err
	}
	ccs, _ := h.AddressList("cc") // ignore "mail:header not in message" error return as it's expected
	bccs, _ := h.AddressList("bcc")

	if len(rcpts) != 1 || len(ccs) != 0 || len(bccs) != 0 {
		// Multiple recipients (to, cc, bcc) would require the html to be encoded for EACH recipient and exploded into n messages, which is TODO.
		return errors.New("This tracking implementation is designed for simple single-recipient messages only, sorry")
	}
	// See if we already have a message ID header; otherwise generate and add it
	uniq := h.Get(SparkPostMessageIDHeader)
	if uniq == "" {
		uniq = UniqMessageID()
		h[SparkPostMessageIDHeader] = []string{uniq} // Add unique value into the message headers for PowerMTA / Signals to process
	}
	wrap.SetMessageInfo(uniq, rcpts[0].Address)
	return nil
}

// write m headers to dst. The m.Header map does not preserve order, but that should not matter.
func writeMessageHeaders(dst io.Writer, h mail.Header) error {
	for hdrType, hdrList := range h {
		for _, hdrVal := range hdrList {
			hdrLine := hdrType + ": " + hdrVal + smtpCRLF
			_, err := io.WriteString(dst, hdrLine)
			if err != nil {
				return err
			}
		}
	}
	// Blank line denotes end of headers
	_, err := io.WriteString(dst, smtpCRLF)
	return err
}

// HandleMessagePart walks the MIME structure, and may be called recursively. The incoming
// content type and cte (content transfer encoding) are passed separately
func (wrap *Wrapper) HandleMessagePart(dst io.Writer, part io.Reader, cType string, cte string) error {
	// Check what MIME media type we have
	mediaType, params, err := mime.ParseMediaType(cType)
	if err != nil {
		// if no media type, defensively handle as per plain, i.e. pass through
		return handlePlainPart(dst, part)
	}
	if strings.HasPrefix(mediaType, "text/html") {
		// Insert decoder into incoming part, and encoder into dst. Quoted-Printable is automatically handled
		// by the reader, no need to handle here: https://golang.org/src/mime/multipart/multipart.go?s=825:1710#L25
		if cte == "base64" {
			part = base64.NewDecoder(base64.StdEncoding, part)
			// pass output through base64 encoding -> line splitter
			lsWriter := smtpproxy.NewLineSplitterWriter(76, []byte("\r\n"), dst)
			dst = base64.NewEncoder(base64.StdEncoding, lsWriter)
		} else {
			if !(cte == "" || cte == "7bit" || cte == "8bit") {
				log.Println("Warning: don't know how to handle Content-Type-Encoding", cte)
			}
		}
		_, err = wrap.TrackHTML(dst, part) // Wrap the links and add tracking pixels (if active)
	} else {
		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(part, params["boundary"])
			err = wrap.handleMultiPart(dst, mr, params["boundary"])
		} else {
			// Everything else such as text/plain, image/gif etc pass through
			err = handlePlainPart(dst, part)
		}
	}
	return err
}

// Transfer through a plain MIME part
func handlePlainPart(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src) // Passthrough
	return err
}

// Transfer through a multipart message, handling recursively as needed.
// 	Based on https://golang.org/pkg/mime/multipart/#example_NewReader
func (wrap *Wrapper) handleMultiPart(dst io.Writer, mr *multipart.Reader, bound string) error {
	pWrt := multipart.NewWriter(dst)
	pWrt.SetBoundary(bound)
	for {
		p, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				err = nil // Usual termination
				break
			}
			return err // Unexpected error
		}
		pWrt2, err := pWrt.CreatePart(p.Header)
		if err != nil {
			return err
		}
		cType := p.Header.Get("Content-Type")
		cte := p.Header.Get("Content-Transfer-Encoding")
		err = wrap.HandleMessagePart(pWrt2, p, cType, cte)
		if err != nil {
			return err
		}
	}
	pWrt.Close() // Put the boundary in
	return nil
}
