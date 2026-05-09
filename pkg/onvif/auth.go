package onvif

import (
	"crypto/sha1"
	"encoding/base64"
)

// ValidateUsernameToken parses a WS-Security UsernameToken from a SOAP envelope
// and returns true iff the digest matches the expected username/password.
//
// Per oasis-200401-wss-username-token-profile-1.0:
//
//	PasswordDigest = Base64(SHA1(Nonce + Created + Password))
//
// The Nonce is base64-decoded before hashing; Created is hashed verbatim as
// it appears in the XML. Both fields come straight out of FindTagValue, which
// regex-strips namespace prefixes so wsse:/wsu: prefixes don't matter.
//
// Empty `expectedPassword` means auth is not required — caller should check
// that before invoking this function. This helper does not enforce policy,
// only verifies the digest.
//
// Replay protection (timestamp window, nonce caching) is intentionally not
// implemented. The bridge runs on a private VLAN behind UFW; the threat model
// is "stop someone on the same LAN segment from typing admin/anything", not
// "defend against active replay attacks from a sophisticated adversary".
func ValidateUsernameToken(body []byte, expectedUsername, expectedPassword string) bool {
	user := FindTagValue(body, "Username")
	digest := FindTagValue(body, "Password")
	nonceB64 := FindTagValue(body, "Nonce")
	created := FindTagValue(body, "Created")

	if user == "" || digest == "" || nonceB64 == "" || created == "" {
		return false
	}

	if user != expectedUsername {
		return false
	}

	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return false
	}

	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(expectedPassword))
	expected := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return expected == digest
}

// AuthFaultEnvelope returns a SOAP 1.2 fault envelope suitable for an ONVIF
// authentication failure. Real cameras return this shape on bad/missing
// credentials and clients (Nx Witness etc.) parse the subcode to decide
// whether to retry with auth.
func AuthFaultEnvelope() []byte {
	return []byte(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
<s:Body>
<s:Fault>
<s:Code>
<s:Value>s:Sender</s:Value>
<s:Subcode>
<s:Value>wsse:FailedAuthentication</s:Value>
</s:Subcode>
</s:Code>
<s:Reason>
<s:Text xml:lang="en">Sender not Authorized</s:Text>
</s:Reason>
</s:Fault>
</s:Body>
</s:Envelope>`)
}
