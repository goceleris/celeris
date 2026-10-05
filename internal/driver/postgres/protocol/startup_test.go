package protocol

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// helper: build a backend message with a type byte, int32 length, payload.
func buildMsg(t byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], uint32(4+len(payload)))
	copy(out[5:], payload)
	return out
}

// helper: build an Authentication message with subtype + body.
func buildAuth(subtype int32, body []byte) []byte {
	payload := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(payload[:4], uint32(subtype))
	copy(payload[4:], body)
	return buildMsg(BackendAuthentication, payload)
}

func TestStartupTrust(t *testing.T) {
	s := &StartupState{User: "alice", Database: "test"}
	w := NewWriter()
	initial := s.Start(w)
	if len(initial) < 8 {
		t.Fatalf("initial message too short: %d", len(initial))
	}
	// Verify user CString appears.
	if !bytes.Contains(initial, []byte("user\x00alice\x00")) {
		t.Fatalf("startup payload missing user: %q", initial)
	}
	if !bytes.Contains(initial, []byte("database\x00test\x00")) {
		t.Fatalf("startup payload missing database")
	}

	// AuthenticationOk.
	msg := buildAuth(AuthOK, nil)
	_, t0, p, err := readOne(msg)
	if err != nil {
		t.Fatal(err)
	}
	_, done, err := s.Handle(t0, p, w)
	if err != nil || done {
		t.Fatalf("after AuthOk: done=%v err=%v", done, err)
	}

	// ParameterStatus server_version=16
	ps := []byte("server_version\x0016.0\x00")
	_, t1, p1, _ := readOne(buildMsg(BackendParameterStatus, ps))
	if _, _, err := s.Handle(t1, p1, w); err != nil {
		t.Fatal(err)
	}
	if s.ServerParams["server_version"] != "16.0" {
		t.Fatalf("server_version=%q", s.ServerParams["server_version"])
	}

	// BackendKeyData.
	bkd := make([]byte, 8)
	binary.BigEndian.PutUint32(bkd[:4], 1234)
	binary.BigEndian.PutUint32(bkd[4:], 5678)
	_, t2, p2, _ := readOne(buildMsg(BackendBackendKeyData, bkd))
	if _, _, err := s.Handle(t2, p2, w); err != nil {
		t.Fatal(err)
	}
	if s.PID != 1234 || s.Secret != 5678 {
		t.Fatalf("bkd: pid=%d secret=%d", s.PID, s.Secret)
	}

	// ReadyForQuery.
	_, t3, p3, _ := readOne(buildMsg(BackendReadyForQuery, []byte{'I'}))
	_, done, err = s.Handle(t3, p3, w)
	if err != nil || !done {
		t.Fatalf("expected done after RFQ, done=%v err=%v", done, err)
	}
}

func TestStartupCleartext(t *testing.T) {
	s := &StartupState{User: "u", Password: "secret"}
	w := NewWriter()
	_ = s.Start(w)

	_, mt, pl, _ := readOne(buildAuth(AuthCleartextPassword, nil))
	resp, done, err := s.Handle(mt, pl, w)
	if err != nil || done {
		t.Fatalf("cleartext: err=%v done=%v", err, done)
	}
	if resp[0] != MsgPasswordMessage {
		t.Fatalf("expected PasswordMessage, got %q", resp[0])
	}
	// payload should be "secret\x00".
	payload := resp[5:]
	if string(payload) != "secret\x00" {
		t.Fatalf("cleartext payload=%q", payload)
	}
}

func TestStartupMD5(t *testing.T) {
	password, user := "pencil", "alice"
	salt := []byte{0x01, 0x02, 0x03, 0x04}
	s := &StartupState{User: user, Password: password}
	w := NewWriter()
	_ = s.Start(w)

	_, mt, pl, _ := readOne(buildAuth(AuthMD5Password, salt))
	resp, done, err := s.Handle(mt, pl, w)
	if err != nil || done {
		t.Fatalf("md5: err=%v done=%v", err, done)
	}
	// Expected: "md5" + hex(md5(hex(md5(password+user))+salt))
	inner := md5.Sum([]byte(password + user))
	ih := hex.EncodeToString(inner[:])
	h := md5.New()
	h.Write([]byte(ih))
	h.Write(salt)
	want := "md5" + hex.EncodeToString(h.Sum(nil)) + "\x00"
	got := string(resp[5:])
	if got != want {
		t.Fatalf("md5 response: got %q want %q", got, want)
	}
}

func TestStartupSCRAMRFC5802Vector(t *testing.T) {
	// RFC 5802 §5 reference uses SCRAM-SHA-1; for SCRAM-SHA-256 the
	// canonical test vector comes from RFC 7677 §3:
	//   username = "user"
	//   password = "pencil"
	//   client-nonce = "rOprNGfwEbeRWgbNEkqO"
	//   server-nonce = "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	//   salt = base64("W22ZaJ0SNY7soEsUEjb6gQ==")
	//   iterations = 4096
	// AuthMessage verification happens inside HandleServerFirst; we build
	// that exchange by hand.
	cli := &scramClient{username: "user", password: "pencil",
		clientNonce: []byte("rOprNGfwEbeRWgbNEkqO")}
	first := cli.ClientFirst()
	if string(first) != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first=%q", first)
	}

	serverFirst := []byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	clientFinal, err := cli.HandleServerFirst(serverFirst)
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7677 §3 expected client-final-message:
	expected := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(clientFinal) != expected {
		t.Fatalf("client-final mismatch:\n got: %q\nwant: %q", clientFinal, expected)
	}

	// Server final message from RFC 7677:
	serverFinal := []byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")
	if err := cli.HandleServerFinal(serverFinal); err != nil {
		t.Fatalf("server-final verify: %v", err)
	}
}

func TestStartupSCRAMHandle(t *testing.T) {
	// End-to-end through StartupState. Nonce is freshly random — we can't
	// check the exact final message, but we can drive the state machine
	// against a locally-simulated server.
	s := &StartupState{User: "alice", Password: "pw"}
	w := NewWriter()
	_ = s.Start(w)

	// Server: AuthenticationSASL with mechanism list.
	mechBody := []byte(scramSHA256 + "\x00" + "\x00")
	_, mt, pl, _ := readOne(buildAuth(AuthSASL, mechBody))
	out, done, err := s.Handle(mt, pl, w)
	if err != nil || done {
		t.Fatalf("SASL init: err=%v done=%v", err, done)
	}
	if !bytes.Contains(out, []byte(scramSHA256)) {
		t.Fatal("expected mechanism name in SASLInitialResponse")
	}

	// Simulate server-first using our own random nonce reply.
	// Pull clientNonce from s.sasl.
	clientNonce := string(s.sasl.clientNonce)
	serverNonce := "ServerPartOfTheNonce"
	salt := []byte("saltsaltsaltsaltsaltsaltsaltsalt")
	sfm := []byte("r=" + clientNonce + serverNonce + ",s=" + toB64(salt) + ",i=4096")
	_, mt2, pl2, _ := readOne(buildAuth(AuthSASLContinue, sfm))
	_, done, err = s.Handle(mt2, pl2, w)
	if err != nil || done {
		t.Fatalf("SASLContinue: err=%v done=%v", err, done)
	}

	// Build the expected server-final that our client would accept.
	serverKey := hmacSHA256(s.sasl.saltedPassword, []byte("Server Key"))
	serverSig := hmacSHA256(serverKey, s.sasl.authMessage)
	sff := []byte("v=" + toB64(serverSig))
	_, mt3, pl3, _ := readOne(buildAuth(AuthSASLFinal, sff))
	if _, _, err := s.Handle(mt3, pl3, w); err != nil {
		t.Fatalf("SASLFinal: %v", err)
	}

	// Server sends AuthenticationOk then RFQ.
	_, mt4, pl4, _ := readOne(buildAuth(AuthOK, nil))
	if _, _, err := s.Handle(mt4, pl4, w); err != nil {
		t.Fatal(err)
	}
	_, mt5, pl5, _ := readOne(buildMsg(BackendReadyForQuery, []byte{'I'}))
	_, done, err = s.Handle(mt5, pl5, w)
	if err != nil || !done {
		t.Fatalf("after RFQ: done=%v err=%v", done, err)
	}
}

func TestStartupErrorResponse(t *testing.T) {
	s := &StartupState{User: "alice"}
	w := NewWriter()
	_ = s.Start(w)
	// Build ErrorResponse with severity=FATAL, code=28P01, message=bad pw.
	payload := []byte{'S'}
	payload = append(payload, []byte("FATAL\x00")...)
	payload = append(payload, 'C')
	payload = append(payload, []byte("28P01\x00")...)
	payload = append(payload, 'M')
	payload = append(payload, []byte("invalid password\x00")...)
	payload = append(payload, 0)

	_, mt, pl, _ := readOne(buildMsg(BackendErrorResponse, payload))
	_, done, err := s.Handle(mt, pl, w)
	if err == nil {
		t.Fatalf("expected error from ErrorResponse")
	}
	if done {
		t.Fatalf("done should be false on error")
	}
	pgErr, ok := err.(*PGError)
	if !ok {
		t.Fatalf("expected *PGError, got %T", err)
	}
	if pgErr.Code != "28P01" || !strings.Contains(pgErr.Message, "invalid password") {
		t.Fatalf("unexpected PGError: %+v", pgErr)
	}
}

func TestStartupUnsupportedAuth(t *testing.T) {
	s := &StartupState{User: "alice"}
	w := NewWriter()
	_ = s.Start(w)
	_, mt, pl, _ := readOne(buildAuth(AuthGSS, nil))
	_, _, err := s.Handle(mt, pl, w)
	if err == nil {
		t.Fatal("expected error for GSS")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error did not mention unsupported: %v", err)
	}
}

// readOne feeds one complete wire message through a Reader and returns the
// type, payload, and error. The first int is the number of bytes consumed.
func readOne(msg []byte) (int, byte, []byte, error) {
	r := NewReader()
	r.Feed(msg)
	t, p, err := r.Next()
	return len(msg), t, p, err
}

func toB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
