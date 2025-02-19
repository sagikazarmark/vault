// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package pki

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
)

func pathAcmeRevoke(b *backend) []*framework.Path {
	return buildAcmeFrameworkPaths(b, patternAcmeRevoke, "/revoke-cert")
}

func patternAcmeRevoke(b *backend, pattern string) *framework.Path {
	fields := map[string]*framework.FieldSchema{}
	addFieldsForACMEPath(fields, pattern)
	addFieldsForACMERequest(fields)

	return &framework.Path{
		Pattern: pattern,
		Fields:  fields,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback:                    b.acmeParsedWrapper(b.acmeRevocationHandler),
				ForwardPerformanceSecondary: false,
				ForwardPerformanceStandby:   true,
			},
		},

		HelpSynopsis:    pathAcmeHelpSync,
		HelpDescription: pathAcmeHelpDesc,
	}
}

func (b *backend) acmeRevocationHandler(acmeCtx *acmeContext, r *logical.Request, fields *framework.FieldData, userCtx *jwsCtx, data map[string]interface{}) (*logical.Response, error) {
	var cert *x509.Certificate

	rawCertificate, present := data["certificate"]
	if present {
		certBase64, ok := rawCertificate.(string)
		if !ok {
			return nil, fmt.Errorf("invalid type (%T; expected string) for field 'certificate': %w", rawCertificate, ErrMalformed)
		}

		certBytes, err := base64.RawURLEncoding.DecodeString(certBase64)
		if err != nil {
			return nil, fmt.Errorf("failed to base64 decode certificate: %v: %w", err, ErrMalformed)
		}

		cert, err = x509.ParseCertificate(certBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse certificate: %v: %w", err, ErrMalformed)
		}
	} else {
		return nil, fmt.Errorf("bad request was lacking required field 'certificate': %w", ErrMalformed)
	}

	rawReason, present := data["reason"]
	if present {
		reason, ok := rawReason.(float64)
		if !ok {
			return nil, fmt.Errorf("invalid type (%T; expected float64) for field 'reason': %w", rawReason, ErrMalformed)
		}

		if int(reason) != 0 {
			return nil, fmt.Errorf("Vault does not support revocation reasons (got %v; expected omitted or 0/unspecified): %w", int(reason), ErrBadRevocationReason)
		}
	}

	// If the certificate expired, there's no point in revoking it.
	if cert.NotAfter.Before(time.Now()) {
		return nil, fmt.Errorf("refusing to revoke expired certificate: %w", ErrMalformed)
	}

	// Fetch the CRL config as we need it to ultimately do the
	// revocation. This should be cached and thus relatively fast.
	config, err := b.crlBuilder.getConfigWithUpdate(acmeCtx.sc)
	if err != nil {
		return nil, fmt.Errorf("unable to revoke certificate: failed reading revocation config: %v: %w", err, ErrServerInternal)
	}

	// Load our certificate from storage to ensure it exists and matches
	// what was given to us.
	serial := serialFromCert(cert)
	certEntry, err := fetchCertBySerial(acmeCtx.sc, "certs/", serial)
	if err != nil {
		return nil, fmt.Errorf("unable to revoke certificate: err reading global cert entry: %v: %w", err, ErrServerInternal)
	}
	if certEntry == nil {
		return nil, fmt.Errorf("unable to revoke certificate: no global cert entry found: %w", ErrServerInternal)
	}

	// Validate that the provided certificate matches the stored
	// certificate. This completes the chain of:
	//
	//     provided_auth -> provided_cert == stored cert.
	//
	// Allowing revocation to be safe.
	//
	// We use the non-subtle unsafe bytes equality check here as we have
	// already fetched this certificate from storage, thus already leaking
	// timing information that this cert exists. The user could thus simply
	// fetch the cert from Vault matching this serial number via the unauthed
	// pki/certs/:serial API endpoint.
	if !bytes.Equal(certEntry.Value, cert.Raw) {
		return nil, fmt.Errorf("unable to revoke certificate: supplied certificate does not match CA's stored value: %w", ErrMalformed)
	}

	// Check if it was already revoked; in this case, we do not need to
	// revoke it again and want to respond with an appropriate error message.
	revEntry, err := fetchCertBySerial(acmeCtx.sc, "revoked/", serial)
	if err != nil {
		return nil, fmt.Errorf("unable to revoke certificate: err reading revocation entry: %v: %w", err, ErrServerInternal)
	}
	if revEntry != nil {
		return nil, fmt.Errorf("unable to revoke certificate: %w", ErrAlreadyRevoked)
	}

	// Finally, do the relevant permissions/authorization check as
	// appropriate based on the type of revocation happening.
	if !userCtx.Existing {
		return b.acmeRevocationByPoP(acmeCtx, r, fields, userCtx, data, cert, config)
	}

	return b.acmeRevocationByAccount(acmeCtx, r, fields, userCtx, data, cert, config)
}

func (b *backend) acmeRevocationByPoP(acmeCtx *acmeContext, r *logical.Request, fields *framework.FieldData, userCtx *jwsCtx, data map[string]interface{}, cert *x509.Certificate, config *crlConfig) (*logical.Response, error) {
	// Since this account does not exist, ensure we've gotten a private key
	// matching the certificate's public key.
	signer, ok := userCtx.Key.Key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unable to revoke certificate: unable to parse JWS key of type (%T): %w", userCtx.Key.Key, ErrMalformed)
	}

	// Ensure that our PoP is indeed valid.
	if err := validatePrivateKeyMatchesCert(signer, cert); err != nil {
		return nil, fmt.Errorf("unable to revoke certificate: unable to verify proof of possession: %v: %w", err, ErrMalformed)
	}

	// Now it is safe to revoke.
	b.revokeStorageLock.Lock()
	defer b.revokeStorageLock.Unlock()

	return revokeCert(acmeCtx.sc, config, cert)
}

func (b *backend) acmeRevocationByAccount(acmeCtx *acmeContext, r *logical.Request, fields *framework.FieldData, userCtx *jwsCtx, data map[string]interface{}, cert *x509.Certificate, config *crlConfig) (*logical.Response, error) {
	// Fetch the account; disallow revocations from non-valid-status
	// accounts.
	account, err := b.acmeState.LoadAccount(acmeCtx, userCtx.Kid)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup account: %w", err)
	}
	if account.Status != AccountStatusValid {
		return nil, fmt.Errorf("account isn't presently valid: %w", ErrUnauthorized)
	}

	// We only support certificates issued by this user, we don't support
	// cross-account revocations.
	serial := serialFromCert(cert)
	acmeEntry, err := b.acmeState.GetIssuedCert(acmeCtx, userCtx.Kid, serial)
	if err != nil || acmeEntry == nil {
		return nil, fmt.Errorf("unable to revoke certificate: %v: %w", err, ErrMalformed)
	}

	// Now it is safe to revoke.
	b.revokeStorageLock.Lock()
	defer b.revokeStorageLock.Unlock()

	return revokeCert(acmeCtx.sc, config, cert)
}
