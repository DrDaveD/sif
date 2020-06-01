// Copyright (c) 2020, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the LICENSE.md file
// distributed with the sources of this project regarding your rights to use or distribute this
// software.

package integrity

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/sylabs/sif/pkg/sif"
	"golang.org/x/crypto/openpgp"
)

var errFingerprintMismatch = errors.New("fingerprint in descriptor does not correspond to signing entity")

type VerifyResult interface {
	// Signature returns the ID of the signature object associated with the result.
	Signature() uint32

	// Signed returns the IDs of data objects that were signed.
	Signed() []uint32

	// Verified returns the IDs of data objects that were verified.
	Verified() []uint32

	// Entity returns the signing entity, or nil if the signing entity could not be determined.
	Entity() *openpgp.Entity

	// Error returns an error describing the reason verification failed, or nil if verification was
	// successful.
	Error() error
}

// VerifyCallback is called immediately after a signature is verified. If r contains a non-nil
// error, and the callback returns true, the error is ignored, and verification proceeds as if no
// error occurred.
type VerifyCallback func(r VerifyResult) (ignoreError bool)

type groupVerifier struct {
	f        *sif.FileImage    // SIF image to verify.
	cb       VerifyCallback    // Verification callback.
	groupID  uint32            // Object group ID.
	ods      []*sif.Descriptor // Object descriptors.
	subsetOK bool              // If true, permit ods to be a subset of the objects in signatures.
}

// newGroupVerifier constructs a new group verifier, optionally limited to objects described by
// ods. If no descriptors are supplied, verify all objects in group.
func newGroupVerifier(f *sif.FileImage, cb VerifyCallback, groupID uint32, ods ...*sif.Descriptor) (*groupVerifier, error) { // nolint:lll
	v := groupVerifier{f: f, cb: cb, groupID: groupID, ods: ods}

	if len(ods) == 0 {
		ods, err := getGroupObjects(f, groupID)
		if err != nil {
			return nil, err
		}
		v.ods = ods
	} else {
		v.subsetOK = true
	}

	return &v, nil
}

// verifySignature verifies the objects specified by v against signature sig using keyring kr.
func (v *groupVerifier) verifySignature(sig *sif.Descriptor, kr openpgp.KeyRing) (imageMetadata, []uint32, *openpgp.Entity, error) { // nolint:lll
	// Verify signature and decode image metadata.
	var im imageMetadata
	e, _, err := verifyAndDecodeJSON(sig.GetData(v.f), &im, kr)
	if err != nil {
		return im, nil, e, err
	}

	// Ensure signing entity matches fingerprint in descriptor.
	fp, err := sig.GetEntity()
	if err != nil {
		return im, nil, e, err
	}
	if !bytes.Equal(e.PrimaryKey.Fingerprint[:], fp[:20]) {
		return im, nil, e, errFingerprintMismatch
	}

	// If an object subset is not permitted, verify our set of IDs match exactly what is in the
	// image metadata.
	if !v.subsetOK {
		if err := im.objectIDsMatch(v.ods); err != nil {
			return im, nil, e, err
		}
	}

	// Verify header and object integrity.
	verified, err := im.matches(v.f, v.ods)
	if err != nil {
		return im, verified, e, err
	}

	return im, verified, e, nil
}

// verifyWithKeyRing performs validation of the objects specified by v using keyring kr.
func (v *groupVerifier) verifyWithKeyRing(kr openpgp.KeyRing) error {
	// Obtain all signatures related to group.
	sigs, err := getGroupSignatures(v.f, v.groupID, false)
	if err != nil {
		return err
	}

	for _, sig := range sigs {
		im, verified, e, err := v.verifySignature(sig, kr)

		// Call verify callback, if applicable.
		if v.cb != nil {
			r := result{signature: sig.ID, im: im, verified: verified, e: e, err: err}
			if ignoreError := v.cb(r); ignoreError {
				err = nil
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

type legacyGroupVerifier struct {
	f       *sif.FileImage    // SIF image to verify.
	cb      VerifyCallback    // Verification callback.
	groupID uint32            // Object group ID.
	ods     []*sif.Descriptor // Object descriptors.
}

// newLegacyGroupVerifier constructs a new legacy group verifier.
func newLegacyGroupVerifier(f *sif.FileImage, cb VerifyCallback, groupID uint32) (*legacyGroupVerifier, error) {
	ods, err := getGroupObjects(f, groupID)
	if err != nil {
		return nil, err
	}
	return &legacyGroupVerifier{f: f, cb: cb, groupID: groupID, ods: ods}, nil
}

// verifySignature verifies the objects specified by v against signature sig using keyring kr.
func (v *legacyGroupVerifier) verifySignature(sig *sif.Descriptor, kr openpgp.KeyRing) (*openpgp.Entity, error) {
	// Verify signature and decode plaintext.
	e, b, _, err := verifyAndDecode(sig.GetData(v.f), kr)
	if err != nil {
		return e, err
	}

	// Ensure signing entity matches fingerprint in descriptor.
	fp, err := sig.GetEntity()
	if err != nil {
		return e, err
	}
	if !bytes.Equal(e.PrimaryKey.Fingerprint[:], fp[:20]) {
		return e, errFingerprintMismatch
	}

	// Determine hash type.
	ht, err := sig.GetHashType()
	if err != nil {
		return e, err
	}

	// Obtain digest from plaintext.
	d, err := newLegacyDigest(ht, b)
	if err != nil {
		return e, err
	}

	// Get reader covering all non-signature objects.
	rs := make([]io.Reader, 0, len(v.ods))
	for _, od := range v.ods {
		// TODO: use something more efficient than GetData.
		rs = append(rs, bytes.NewReader(od.GetData(v.f)))
	}
	r := io.MultiReader(rs...)

	// Verify header and object integrity.
	if ok, err := d.matches(r); err != nil {
		return e, err
	} else if !ok {
		return e, fmt.Errorf("group %d: %w", v.groupID, errObjectIntegrity)
	}

	return e, nil
}

// verifyWithKeyRing performs validation of the objects specified by v using keyring kr.
func (v *legacyGroupVerifier) verifyWithKeyRing(kr openpgp.KeyRing) error {
	// Obtain all signatures related to object.
	sigs, err := getGroupSignatures(v.f, v.groupID, true)
	if err != nil {
		return err
	}

	for _, sig := range sigs {
		e, err := v.verifySignature(sig, kr)

		// Call verify callback, if applicable.
		if v.cb != nil {
			r := legacyResult{signature: sig.ID, ods: v.ods, e: e, err: err}
			if ignoreError := v.cb(r); ignoreError {
				err = nil
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

type legacyObjectVerifier struct {
	f  *sif.FileImage  // SIF image to verify.
	cb VerifyCallback  // Verification callback.
	od *sif.Descriptor // Object descriptor.
}

// newLegacyObjectVerifier constructs a new legacy object verifier.
func newLegacyObjectVerifier(f *sif.FileImage, cb VerifyCallback, id uint32) (*legacyObjectVerifier, error) {
	od, err := getObject(f, id)
	if err != nil {
		return nil, err
	}
	return &legacyObjectVerifier{f: f, cb: cb, od: od}, nil
}

// verifySignature verifies the objects specified by v against signature sig using keyring kr.
func (v *legacyObjectVerifier) verifySignature(sig *sif.Descriptor, kr openpgp.KeyRing) (*openpgp.Entity, error) {
	// Verify signature and decode plaintext.
	e, b, _, err := verifyAndDecode(sig.GetData(v.f), kr)
	if err != nil {
		return e, err
	}

	// Ensure signing entity matches fingerprint in descriptor.
	fp, err := sig.GetEntity()
	if err != nil {
		return e, err
	}
	if !bytes.Equal(e.PrimaryKey.Fingerprint[:], fp[:20]) {
		return e, errFingerprintMismatch
	}

	// Determine hash type.
	ht, err := sig.GetHashType()
	if err != nil {
		return e, err
	}

	// Obtain digest from plaintext.
	d, err := newLegacyDigest(ht, b)
	if err != nil {
		return e, err
	}

	// TODO: use something more efficient than GetData.
	r := bytes.NewReader(v.od.GetData(v.f))

	// Verify header and object integrity.
	if ok, err := d.matches(r); err != nil {
		return e, err
	} else if !ok {
		return e, fmt.Errorf("object %d: %w", v.od.ID, errObjectIntegrity)
	}

	return e, nil
}

// verifyWithKeyRing performs validation of the objects specified by v using keyring kr.
func (v *legacyObjectVerifier) verifyWithKeyRing(kr openpgp.KeyRing) error {
	// Obtain all signatures related to object.
	sigs, err := getObjectSignatures(v.f, v.od.ID)
	if err != nil {
		return err
	}

	for _, sig := range sigs {
		e, err := v.verifySignature(sig, kr)

		// Call verify callback, if applicable.
		if v.cb != nil {
			r := legacyResult{signature: sig.ID, ods: []*sif.Descriptor{v.od}, e: e, err: err}
			if ignoreError := v.cb(r); ignoreError {
				err = nil
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

type verifyTask interface {
	verifyWithKeyRing(kr openpgp.KeyRing) error
}

// Verifier describes a SIF image verifier.
type Verifier struct {
	f *sif.FileImage // SIF image to verify.

	keyRing  openpgp.KeyRing // Keyring to use for verification.
	groups   []uint32        // Data object group(s) selected for verification.
	objects  []uint32        // Individual data object(s) selected for verification.
	isLegacy bool            // Enable verification of legacy signature(s).
	cb       VerifyCallback  // Verification callback.

	tasks []verifyTask // Slice of verification tasks.
}

// VerifierOpt are used to configure v.
type VerifierOpt func(v *Verifier) error

// OptVerifyWithKeyRing sets the keyring to use for verification to kr.
func OptVerifyWithKeyRing(kr openpgp.KeyRing) VerifierOpt {
	return func(v *Verifier) error {
		v.keyRing = kr
		return nil
	}
}

// OptVerifyGroup adds a verification task for the group with the specified groupID. This may be
// called multliple times to request verification of more than one group.
func OptVerifyGroup(groupID uint32) VerifierOpt {
	return func(v *Verifier) error {
		if groupID == 0 {
			return errInvalidGroupID
		}
		v.groups = insertSorted(v.groups, groupID)
		return nil
	}
}

// OptVerifyObject adds a verification task for the object with the specified id. This may be
// called multliple times to request verification of more than one object.
func OptVerifyObject(id uint32) VerifierOpt {
	return func(v *Verifier) error {
		if id == 0 {
			return errInvalidObjectID
		}
		v.objects = insertSorted(v.objects, id)
		return nil
	}
}

// OptVerifyLegacy enables verification of legacy signatures. Non-legacy signatures will not be
// considered.
func OptVerifyLegacy() VerifierOpt {
	return func(v *Verifier) error {
		v.isLegacy = true
		return nil
	}
}

// getTasks returns verification tasks corresponding to groupIDs and objectIDs.
func getTasks(f *sif.FileImage, cb VerifyCallback, groupIDs []uint32, objectIDs []uint32) ([]verifyTask, error) {
	t := make([]verifyTask, 0, len(groupIDs)+len(objectIDs))

	for _, groupID := range groupIDs {
		v, err := newGroupVerifier(f, cb, groupID)
		if err != nil {
			return nil, err
		}
		t = append(t, v)
	}

	for _, id := range objectIDs {
		od, err := getObject(f, id)
		if err != nil {
			return nil, err
		}

		v, err := newGroupVerifier(f, cb, od.Groupid&^sif.DescrGroupMask, od)
		if err != nil {
			return nil, err
		}
		t = append(t, v)
	}

	return t, nil
}

// getLegacyTasks returns legacy verification tasks corresponding to groupIDs and objectIDs.
func getLegacyTasks(f *sif.FileImage, cb VerifyCallback, groupIDs []uint32, objectIDs []uint32) ([]verifyTask, error) {
	t := make([]verifyTask, 0, len(groupIDs)+len(objectIDs))

	for _, groupID := range groupIDs {
		v, err := newLegacyGroupVerifier(f, cb, groupID)
		if err != nil {
			return nil, err
		}
		t = append(t, v)
	}

	for _, id := range objectIDs {
		v, err := newLegacyObjectVerifier(f, cb, id)
		if err != nil {
			return nil, err
		}
		t = append(t, v)
	}

	return t, nil
}

// NewVerifier returns a Verifier to examine and/or verify digital signatures(s) in f according to
// opts.
//
// Verify requires key material be provided. OptVerifyWithKeyRing can be used for this purpose.
//
// By default, the returned Verifier will consider non-legacy signatures for all object groups. To
// override this behavior, consider using OptVerifyGroup, OptVerifyObject, and/or OptVerifyLegacy.
func NewVerifier(f *sif.FileImage, opts ...VerifierOpt) (*Verifier, error) {
	if f == nil {
		return nil, fmt.Errorf("integrity: %w", errNilFileImage)
	}

	v := &Verifier{f: f}

	// Apply options.
	for _, o := range opts {
		if err := o(v); err != nil {
			return nil, fmt.Errorf("integrity: %w", err)
		}
	}

	// If no verification tasks specified, add one per object group
	if len(v.groups) == 0 && len(v.objects) == 0 {
		ids, err := getGroupIDs(f)
		if err != nil {
			return nil, fmt.Errorf("integrity: %w", err)
		}
		v.groups = ids
	}

	// Get tasks.
	getTasksFunc := getTasks
	if v.isLegacy {
		getTasksFunc = getLegacyTasks
	}
	t, err := getTasksFunc(f, v.cb, v.groups, v.objects)
	if err != nil {
		return nil, fmt.Errorf("integrity: %w", err)
	}
	v.tasks = t

	return v, nil
}

// Verify performs all cryptographic verification tasks specified by v.
//
// If key material was not provided when v was created, Verify returns an error wrapping
// ErrNoKeyMaterial.
func (v *Verifier) Verify() error {
	if v.keyRing == nil {
		return fmt.Errorf("integrity: %w", ErrNoKeyMaterial)
	}

	for _, t := range v.tasks {
		if err := t.verifyWithKeyRing(v.keyRing); err != nil {
			return fmt.Errorf("integrity: %w", err)
		}
	}
	return nil
}
