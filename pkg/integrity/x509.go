// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
//   For website terms of use, trademark policy, privacy policy and other
//   project policies see https://lfprojects.org/policies
// Copyright (c) 2020-2022, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the LICENSE.md file
// distributed with the sources of this project regarding your rights to use or distribute this
// software.

package integrity

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ocsp"
)

type X509Signer struct {
	Signer      crypto.Signer
	Certificate *x509.Certificate
}

func GetX509Signer(privKeyPath string, certPath string) (*X509Signer, error) {
	certs, err := NewChainedCertificates(certPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to X509 certificate")
	}

	signer, err := GetPKCS8Key(privKeyPath)
	if err != nil {
		return nil, err
	}

	signerCertificate, err := certs.GetCertificate()
	if err != nil {
		return nil, err
	}

	return &X509Signer{
		Signer:      signer,
		Certificate: signerCertificate,
	}, nil
}

func GetPKCS8Key(filepath string) (crypto.Signer, error) {
	// open raw data
	rawPrivKey, err := os.ReadFile(filepath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to PKCS8 private key")
	}

	var tryWith []byte

	// try to decode raw data as PEM
	block, _ := pem.Decode(rawPrivKey)
	if block == nil || block.Type != "PRIVATE KEY" {
		tryWith = rawPrivKey
	} else {
		tryWith = block.Bytes
	}

	// decode raw data as DER
	key, err := x509.ParsePKCS8PrivateKey(tryWith)
	if err != nil {
		return nil, errors.Errorf("failed to decode private key")
	}

	return key.(crypto.Signer), nil
}

/*****************************************
	Intermediate / RootCA Certificates
*****************************************/

type ChainedCertificates map[string]*x509.Certificate

func NewChainedCertificates(filepath string) (ChainedCertificates, error) {
	// If user options are not defined, use the default system cert pool
	if filepath == "" {
		return nil, errors.Errorf("empty certificate path")
	}

	pemStructures, err := os.ReadFile(filepath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot read file %s", filepath)
	}

	chain := make(map[string]*x509.Certificate)

	for nextEntry := pemStructures; nextEntry != nil; {
		var tryWith []byte

		// try to decode raw data as PEM
		block, rest := pem.Decode(nextEntry)
		if block == nil {
			return chain, nil
		}

		if block.Type == "CERTIFICATE" {
			// Extract CERTIFICATE DER from PEM
			tryWith = block.Bytes
		} else {
			// Try to decode DER directly
			tryWith = nextEntry
		}

		// decode raw data as DER
		cert, err := x509.ParseCertificate(tryWith)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to decode certificate from %s", filepath)
		}

		// add certs to the chain
		chain[string(cert.SubjectKeyId)] = cert

		// go to next entry
		nextEntry = rest
	}

	return chain, nil
}

func (chain ChainedCertificates) GetCertificate() (*x509.Certificate, error) {
	if len(chain) != 1 {
		return nil, errors.Errorf("Expected 1 certificate but found '%d'", len(chain))
	}

	for _, cert := range chain {
		return cert, nil
	}

	panic("should not happen")
}

func (chain ChainedCertificates) GetCertPool() *x509.CertPool {
	pool := x509.NewCertPool()

	for _, cert := range chain {
		pool.AddCert(cert)
	}

	return pool
}

const (
	PKIXOCSPNoCheck = "1.3.6.1.5.5.7.48.1.5"
)

func (chain ChainedCertificates) Verify(intermediateCerts, rootCerts ChainedCertificates) error {
	if len(chain) == 0 {
		return errors.Errorf("there must be at least one certificate to verify")
	}

	var rootCAs *x509.CertPool

	if len(rootCerts) == 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return errors.Wrapf(err, "cannot create system cert pool")
		}

		rootCAs = pool
	} else {
		// ensure that all trusted certs are CA
		for _, cert := range rootCerts {
			if !cert.IsCA {
				return errors.Errorf("trusted certificate may belong only to a Root CA")
			}
		}

		rootCAs = rootCerts.GetCertPool()
	}

	// Offline verification of certificate chain
	vOpts := x509.VerifyOptions{
		DNSName:       "",
		Intermediates: intermediateCerts.GetCertPool(),
		Roots:         rootCAs,
		CurrentTime:   time.Now(),
		// KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageEmailProtection},
		KeyUsages:                 []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		MaxConstraintComparisions: 0,
	}

	for _, cert := range chain {
		for _, extension := range cert.Extensions {
			if extension.Id.String() == PKIXOCSPNoCheck {
				// The CA requires us to explicitly trust this certificate
				// RFC-6960 Section: 4.2.2.2.1
				goto skipVerify
			}
		}

		if _, err := cert.Verify(vOpts); err != nil {
			return errors.Wrapf(err, "failed to verify certificate '%s'", cert.Subject)
		}

	skipVerify:
	}

	return nil
}

func (chain ChainedCertificates) RevocationCheck(intermediateCerts, rootCerts ChainedCertificates) error {
	check := func(cert *x509.Certificate) error {
		// the certificate is self-signed.
		if string(cert.AuthorityKeyId) == string(cert.SubjectKeyId) {
			return nil
		}

		// firstly, look for the issuer in the intermediate certificates.
		issuer, exists := intermediateCerts[string(cert.AuthorityKeyId)]
		if !exists {
			// if not found, look for the issuer on the root certificates.
			issuer, exists = rootCerts[string(cert.AuthorityKeyId)]
			if !exists {
				// if not found anywhere locally, try to download it
				missingCerts, err := rootCerts.AppendCertFromURLs(cert.IssuingCertificateURL...)
				if err != nil {
					return errors.Wrapf(err, "download cert error")
				}

				// if that does not work either, just abort
				issuer, exists = missingCerts[string(cert.AuthorityKeyId)]
				if !exists {
					return errors.Errorf("cannot find issuer  '%s' for '%s'", cert.Issuer, cert.Subject)
				}
			}
		}

		// check the OCSP response
		ocspChain, err := checkRevocation(cert, issuer)
		if err != nil {
			return errors.Wrapf(err, "verify subject '%s' from issuer '%s' ", cert.Subject, cert.Issuer)
		}

		// make sure that the OCSP server is trustworthy
		if len(ocspChain) > 0 {
			if err := ocspChain.Verify(intermediateCerts, rootCerts); err != nil {
				return errors.Wrapf(err, "cannot verify OCSP server's certificate")
			}
		}

		return nil
	}

	// verify certs at this level
	for _, cert := range chain {
		if err := check(cert); err != nil {
			return err
		}
	}

	// verify intermediate certificates
	for _, cert := range intermediateCerts {
		if err := check(cert); err != nil {
			return err
		}
	}

	return nil
}

func (chain ChainedCertificates) AppendCertFromURLs(urls ...string) (ChainedCertificates, error) {
	for _, certURL := range urls {
		//nolint:gosec
		// Alternative:  GOSEC=gosec -quiet -exclude=G104,G107
		resp, err := http.Get(certURL)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot get certificate from '%s'", certURL)
		}

		defer resp.Body.Close()

		caCertBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot read CA certificate's body frin '%s'", certURL)
		}

		// decode raw data as DER
		cert, err := x509.ParseCertificate(caCertBody)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to decode certificate from '%s'", certURL)
		}

		// add certs to the chain
		chain[string(cert.SubjectKeyId)] = cert
	}

	return chain, nil
}

func (chain ChainedCertificates) AppendCerts(certs ...*x509.Certificate) ChainedCertificates {
	for _, cert := range certs {
		chain[string(cert.SubjectKeyId)] = cert
	}

	return chain
}

func checkRevocation(cert, issuer *x509.Certificate) (ChainedCertificates, error) {
	ocspChain := ChainedCertificates{}

	// Parse OCSP Server
	ocspURL, err := url.Parse(cert.OCSPServer[0])
	if err != nil {
		return ocspChain, errors.Wrapf(err, "canot parse OCSP Server from certificate")
	}

	// Create OCSP Request
	opts := &ocsp.RequestOptions{Hash: crypto.SHA256}

	buffer, err := ocsp.CreateRequest(cert, issuer, opts)
	if err != nil {
		return ocspChain, err
	}

	httpRequest, err := http.NewRequest(http.MethodPost, cert.OCSPServer[0], bytes.NewBuffer(buffer))
	if err != nil {
		return ocspChain, err
	}

	// Submit OCSP Request
	httpRequest.Header.Add("Content-Type", "application/ocsp-request")
	httpRequest.Header.Add("Accept", "application/ocsp-response")
	httpRequest.Header.Add("host", ocspURL.Host)

	httpClient := &http.Client{}
	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return ocspChain, errors.Wrapf(err, "cannot send ocsp request")
	}

	defer httpResponse.Body.Close()

	// Parse OCSP Response
	output, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return ocspChain, errors.Wrapf(err, "cannot read response body")
	}

	ocspResponse, err := ocsp.ParseResponse(output, issuer)
	if err != nil {
		return ChainedCertificates{}, err
	}

	// The OCSP is signed by a third-party issuer that we need to verify.
	if ocspResponse.Certificate != nil {
		ocspChain.AppendCerts(ocspResponse.Certificate)
	}

	// Check validity
	switch ocspResponse.Status {
	case ocsp.Good: // means the certificate is still valid
		return ocspChain, nil

	case ocsp.Revoked: // says the certificate was revoked and cannot be trusted
		return ocspChain, errors.Errorf("certificate revoked at '%s'. Revocation reason code: '%d'",
			ocspResponse.RevokedAt, ocspResponse.RevocationReason)

	default: // states that the server does not know about the requested certificate,
		return ocspChain, errors.Errorf("status unknown. certificate cannot be trusted")
	}
}