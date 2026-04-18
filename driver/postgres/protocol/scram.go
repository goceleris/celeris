package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
)

// scramClient implements the client side of SCRAM-SHA-256 as specified by
// RFC 5802 and used by PostgreSQL (RFC 7677 profile, no channel binding).
//
// The flow is:
//
//	C: client-first-message    = "n,,n=<user>,r=<client-nonce>"
//	S: server-first-message    = "r=<client-nonce||server-nonce>,s=<salt>,i=<iter>"
//	C: client-final-message    = "c=biws,r=<full-nonce>,p=<proof>"
//	S: server-final-message    = "v=<server-signature>"
//
// We do not support channel binding ("y,," / "p=...,,") — the gs2-header
// is fixed at "n,,".
type scramClient struct {
	username string
	password string

	clientNonce []byte // base64-encoded; at least 24 bytes of entropy
	clientFirst []byte // "n,,n=user,r=nonce"
	serverFirst []byte

	salt       []byte
	iterations int
	fullNonce  []byte // client-nonce || server-nonce (as presented by server)

	saltedPassword []byte
	authMessage    []byte // client-first-bare + "," + server-first + "," + client-final-without-proof
}

// newSCRAM builds an scramClient with a freshly-generated client nonce.
func newSCRAM(username, password string) (*scramClient, error) {
	// 24 random bytes -> 32-byte base64 alphabet, well above the RFC's
	// recommended minimum.
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("postgres/protocol: scram nonce: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	return &scramClient{
		username:    username,
		password:    password,
		clientNonce: []byte(enc),
	}, nil
}

// ClientFirst returns the client-first-message bytes and caches them for
// the AuthMessage computation.
func (s *scramClient) ClientFirst() []byte {
	// SASLprep is not applied to the username here — PostgreSQL requires
	// clients to send the connection-string user verbatim in SCRAM
	// (server uses the role name stored in pg_authid, which already did
	// its own preparation at role-create time).
	//
	// The username attribute must escape '=' and ',': "=" -> "=3D",
	// "," -> "=2C".
	user := scramEscape(s.username)
	buf := make([]byte, 0, 32+len(user)+len(s.clientNonce))
	buf = append(buf, 'n', ',', ',', 'n', '=')
	buf = append(buf, user...)
	buf = append(buf, ',', 'r', '=')
	buf = append(buf, s.clientNonce...)
	s.clientFirst = buf
	return buf
}

// HandleServerFirst parses the server-first-message, derives SaltedPassword,
// ClientKey, StoredKey, ClientSignature, ClientProof, and returns the
// client-final-message bytes.
func (s *scramClient) HandleServerFirst(msg []byte) ([]byte, error) {
	s.serverFirst = append(s.serverFirst[:0], msg...)
	attrs, err := parseSCRAMAttrs(msg)
	if err != nil {
		return nil, err
	}
	rAttr, ok := attrs['r']
	if !ok {
		return nil, errors.New("postgres/protocol: server-first missing r=")
	}
	if !bytes.HasPrefix(rAttr, s.clientNonce) {
		return nil, errors.New("postgres/protocol: server nonce does not extend client nonce")
	}
	if len(rAttr) == len(s.clientNonce) {
		return nil, errors.New("postgres/protocol: server did not append nonce bytes")
	}
	s.fullNonce = append([]byte(nil), rAttr...)

	saltAttr, ok := attrs['s']
	if !ok {
		return nil, errors.New("postgres/protocol: server-first missing s=")
	}
	salt, err := base64.StdEncoding.DecodeString(string(saltAttr))
	if err != nil {
		return nil, fmt.Errorf("postgres/protocol: scram salt base64: %w", err)
	}
	s.salt = salt

	iAttr, ok := attrs['i']
	if !ok {
		return nil, errors.New("postgres/protocol: server-first missing i=")
	}
	iter, err := strconv.Atoi(string(iAttr))
	if err != nil || iter < 1 {
		return nil, fmt.Errorf("postgres/protocol: bad scram iteration count: %s", string(iAttr))
	}
	s.iterations = iter

	// SaltedPassword := Hi(password, salt, i)
	s.saltedPassword, err = pbkdf2.Key(sha256.New, s.password, salt, iter, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("postgres/protocol: pbkdf2: %w", err)
	}

	// client-final-without-proof = "c=biws,r=<full-nonce>"
	// biws == base64("n,,")
	var cfwp bytes.Buffer
	cfwp.WriteString("c=biws,r=")
	cfwp.Write(s.fullNonce)

	// AuthMessage = client-first-bare + "," + server-first-message + "," + client-final-without-proof
	clientFirstBare := s.clientFirst[3:] // strip "n,,"
	authMsg := make([]byte, 0, len(clientFirstBare)+1+len(msg)+1+cfwp.Len())
	authMsg = append(authMsg, clientFirstBare...)
	authMsg = append(authMsg, ',')
	authMsg = append(authMsg, msg...)
	authMsg = append(authMsg, ',')
	authMsg = append(authMsg, cfwp.Bytes()...)
	s.authMessage = authMsg

	// ClientKey = HMAC(SaltedPassword, "Client Key")
	clientKey := hmacSHA256(s.saltedPassword, []byte("Client Key"))
	// StoredKey = H(ClientKey)
	storedKey := sha256.Sum256(clientKey)
	// ClientSignature = HMAC(StoredKey, AuthMessage)
	clientSig := hmacSHA256(storedKey[:], authMsg)
	// ClientProof = ClientKey XOR ClientSignature
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ clientSig[i]
	}

	// client-final-message = client-final-without-proof + ",p=" + base64(ClientProof)
	var final bytes.Buffer
	final.Write(cfwp.Bytes())
	final.WriteString(",p=")
	final.WriteString(base64.StdEncoding.EncodeToString(proof))
	return final.Bytes(), nil
}

// HandleServerFinal parses the server-final-message and verifies the
// ServerSignature. A non-nil error means the server failed to authenticate
// itself (or returned an explicit error attribute).
func (s *scramClient) HandleServerFinal(msg []byte) error {
	attrs, err := parseSCRAMAttrs(msg)
	if err != nil {
		return err
	}
	if e, ok := attrs['e']; ok {
		return fmt.Errorf("postgres/protocol: scram server error: %s", string(e))
	}
	v, ok := attrs['v']
	if !ok {
		return errors.New("postgres/protocol: server-final missing v=")
	}
	serverSigSent, err := base64.StdEncoding.DecodeString(string(v))
	if err != nil {
		return fmt.Errorf("postgres/protocol: scram v base64: %w", err)
	}
	// ServerKey = HMAC(SaltedPassword, "Server Key")
	serverKey := hmacSHA256(s.saltedPassword, []byte("Server Key"))
	// ServerSignature = HMAC(ServerKey, AuthMessage)
	serverSig := hmacSHA256(serverKey, s.authMessage)
	if !hmac.Equal(serverSigSent, serverSig) {
		return errors.New("postgres/protocol: scram server signature mismatch")
	}
	return nil
}

// parseSCRAMAttrs splits a comma-separated list of "k=v" attributes into a
// map keyed by the single-letter attribute name. Values may legally contain
// '=' characters (e.g., trailing base64 padding), so we only split on the
// first '=' per attribute.
func parseSCRAMAttrs(msg []byte) (map[byte][]byte, error) {
	out := make(map[byte][]byte, 4)
	for _, part := range bytes.Split(msg, []byte{','}) {
		if len(part) < 2 || part[1] != '=' {
			return nil, fmt.Errorf("postgres/protocol: malformed scram attr %q", part)
		}
		out[part[0]] = part[2:]
	}
	return out, nil
}

// hmacSHA256 returns HMAC-SHA-256(key, data).
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// scramEscape replaces '=' with "=3D" and ',' with "=2C" per RFC 5802 §5.1.
func scramEscape(s string) string {
	// Fast path: nothing to escape.
	if !containsAny(s, "=,") {
		return s
	}
	var b bytes.Buffer
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '=':
			b.WriteString("=3D")
		case ',':
			b.WriteString("=2C")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func containsAny(s, chars string) bool {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return true
			}
		}
	}
	return false
}
