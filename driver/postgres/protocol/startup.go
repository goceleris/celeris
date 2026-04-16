package protocol

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// ProtocolVersion is the v3.0 startup major/minor encoded as a single int32.
const ProtocolVersion int32 = 3 << 16

// Authentication subtypes carried in the 'R' message's int32 body prefix.
const (
	AuthOK                int32 = 0
	AuthKerberosV5        int32 = 2
	AuthCleartextPassword int32 = 3
	AuthMD5Password       int32 = 5
	AuthGSS               int32 = 7
	AuthGSSContinue       int32 = 8
	AuthSSPI              int32 = 9
	AuthSASL              int32 = 10
	AuthSASLContinue      int32 = 11
	AuthSASLFinal         int32 = 12
)

// SCRAM mechanism names advertised in AuthenticationSASL.
const (
	scramSHA256     = "SCRAM-SHA-256"
	scramSHA256Plus = "SCRAM-SHA-256-PLUS" // channel binding — not supported
)

// startupPhase is the state of the StartupState machine.
type startupPhase int

const (
	phaseInit           startupPhase = iota // have not yet sent StartupMessage
	phaseAwaitAuth                          // awaiting AuthenticationOk or an auth challenge
	phaseAwaitSASLCont                      // sent client-first, awaiting server-first
	phaseAwaitSASLFinal                     // sent client-final, awaiting server-final
	phaseAwaitReady                         // auth done, waiting for ReadyForQuery
	phaseReady                              // ReadyForQuery received — startup complete
)

// StartupState drives the client side of the PostgreSQL startup and
// authentication exchange. Callers feed it one received message at a time
// via Handle and send the returned response bytes (if any) back to the
// server. Handle returns done=true after ReadyForQuery.
//
// A StartupState is single-use: once done is true, it must not receive more
// messages. Use a fresh instance per connection.
type StartupState struct {
	User     string
	Password string
	Database string
	Params   map[string]string

	// Populated as the exchange proceeds.
	PID          int32
	Secret       int32
	ServerParams map[string]string

	phase startupPhase
	sasl  *scramClient
}

// Start returns the initial StartupMessage bytes. The Writer is reset first.
// It must be called exactly once at the start of the exchange.
func (s *StartupState) Start(w *Writer) []byte {
	if s.phase != phaseInit {
		panic("postgres/protocol: StartupState.Start called twice")
	}
	if s.ServerParams == nil {
		s.ServerParams = make(map[string]string)
	}
	w.Reset()
	w.StartStartupMessage()
	w.WriteInt32(ProtocolVersion)
	w.WriteString("user")
	w.WriteString(s.User)
	if s.Database != "" {
		w.WriteString("database")
		w.WriteString(s.Database)
	}
	for k, v := range s.Params {
		w.WriteString(k)
		w.WriteString(v)
	}
	// Trailing null byte terminates the parameter list.
	_ = w.WriteByte(0)
	w.FinishMessage()
	s.phase = phaseAwaitAuth
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// Handle consumes one server message. It returns the bytes to transmit (or
// nil), whether the startup exchange has completed, and an error. On error
// the caller should close the connection.
func (s *StartupState) Handle(msgType byte, payload []byte, w *Writer) (response []byte, done bool, err error) {
	switch msgType {
	case BackendErrorResponse:
		return nil, false, ParseErrorResponse(payload)
	case BackendNoticeResponse:
		// Ignore notices during startup.
		return nil, false, nil
	case BackendParameterStatus:
		return nil, false, s.handleParameterStatus(payload)
	case BackendBackendKeyData:
		return nil, false, s.handleBackendKeyData(payload)
	case BackendAuthentication:
		return s.handleAuth(payload, w)
	case BackendReadyForQuery:
		if len(payload) < 1 {
			return nil, false, errors.New("postgres/protocol: short ReadyForQuery")
		}
		s.phase = phaseReady
		return nil, true, nil
	case BackendNegotiateProtocolVersion:
		// Server advertises unsupported options; continue.
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("postgres/protocol: unexpected message %q during startup", msgType)
	}
}

// handleAuth dispatches on the subtype int32.
func (s *StartupState) handleAuth(payload []byte, w *Writer) ([]byte, bool, error) {
	if len(payload) < 4 {
		return nil, false, errors.New("postgres/protocol: short Authentication payload")
	}
	subtype := int32(binary.BigEndian.Uint32(payload[:4]))
	body := payload[4:]
	switch subtype {
	case AuthOK:
		s.phase = phaseAwaitReady
		return nil, false, nil
	case AuthCleartextPassword:
		out := s.writePasswordMessage(w, s.Password)
		return out, false, nil
	case AuthMD5Password:
		if len(body) < 4 {
			return nil, false, errors.New("postgres/protocol: short MD5 salt")
		}
		salt := body[:4]
		out := s.writePasswordMessage(w, md5Hash(s.Password, s.User, salt))
		return out, false, nil
	case AuthSASL:
		mech, err := pickSASLMech(body)
		if err != nil {
			return nil, false, err
		}
		cli, err := newSCRAM(s.User, s.Password)
		if err != nil {
			return nil, false, err
		}
		s.sasl = cli
		first := cli.ClientFirst()
		out := writeSASLInitial(w, mech, first)
		s.phase = phaseAwaitSASLCont
		return out, false, nil
	case AuthSASLContinue:
		if s.sasl == nil {
			return nil, false, errors.New("postgres/protocol: SASLContinue without SASL in progress")
		}
		clientFinal, err := s.sasl.HandleServerFirst(body)
		if err != nil {
			return nil, false, err
		}
		out := writeSASLResponse(w, clientFinal)
		s.phase = phaseAwaitSASLFinal
		return out, false, nil
	case AuthSASLFinal:
		if s.sasl == nil {
			return nil, false, errors.New("postgres/protocol: SASLFinal without SASL in progress")
		}
		if err := s.sasl.HandleServerFinal(body); err != nil {
			return nil, false, err
		}
		// SASLFinal is followed by AuthenticationOk from the server.
		return nil, false, nil
	case AuthKerberosV5, AuthGSS, AuthGSSContinue, AuthSSPI:
		return nil, false, errors.New("postgres/protocol: unsupported auth method (GSS/SSPI/Kerberos); see v1.4.x roadmap")
	default:
		return nil, false, fmt.Errorf("postgres/protocol: unknown Authentication subtype %d", subtype)
	}
}

// pickSASLMech selects SCRAM-SHA-256 from the server-advertised list. The
// body is a sequence of CStrings terminated by an empty CString.
func pickSASLMech(body []byte) (string, error) {
	pos := 0
	for pos < len(body) {
		s, err := ReadCString(body, &pos)
		if err != nil {
			return "", err
		}
		if s == "" {
			break
		}
		if s == scramSHA256 {
			return s, nil
		}
	}
	return "", errors.New("postgres/protocol: server did not advertise SCRAM-SHA-256 (channel-binding variants not supported)")
}

func (s *StartupState) handleParameterStatus(payload []byte) error {
	pos := 0
	name, err := ReadCString(payload, &pos)
	if err != nil {
		return err
	}
	val, err := ReadCString(payload, &pos)
	if err != nil {
		return err
	}
	if s.ServerParams == nil {
		s.ServerParams = make(map[string]string)
	}
	s.ServerParams[name] = val
	return nil
}

func (s *StartupState) handleBackendKeyData(payload []byte) error {
	if len(payload) < 8 {
		return errors.New("postgres/protocol: short BackendKeyData")
	}
	s.PID = int32(binary.BigEndian.Uint32(payload[:4]))
	s.Secret = int32(binary.BigEndian.Uint32(payload[4:8]))
	return nil
}

// writePasswordMessage encodes a PasswordMessage ('p') carrying the given
// cleartext or pre-hashed password. The password is nul-terminated.
func (s *StartupState) writePasswordMessage(w *Writer, password string) []byte {
	w.Reset()
	w.StartMessage(MsgPasswordMessage)
	w.WriteString(password)
	w.FinishMessage()
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// writeSASLInitial encodes a SASLInitialResponse: mechanism name (CString)
// + int32 length of client-first-message + client-first-message bytes.
func writeSASLInitial(w *Writer, mechanism string, clientFirst []byte) []byte {
	w.Reset()
	w.StartMessage(MsgPasswordMessage)
	w.WriteString(mechanism)
	w.WriteInt32(int32(len(clientFirst)))
	w.WriteBytes(clientFirst)
	w.FinishMessage()
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// writeSASLResponse encodes a SASLResponse: raw message bytes in a
// PasswordMessage frame.
func writeSASLResponse(w *Writer, msg []byte) []byte {
	w.Reset()
	w.StartMessage(MsgPasswordMessage)
	w.WriteBytes(msg)
	w.FinishMessage()
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// md5Hash computes the MD5-password response:
//
//	"md5" + hex(md5(hex(md5(password + user)) + salt))
func md5Hash(password, user string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	innerHex := hex.EncodeToString(inner[:])
	h := md5.New()
	h.Write([]byte(innerHex))
	h.Write(salt)
	return "md5" + hex.EncodeToString(h.Sum(nil))
}
