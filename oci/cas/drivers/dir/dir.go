/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016, 2017 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package dir

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"

	"github.com/openSUSE/umoci/oci/cas"
	"github.com/openSUSE/umoci/pkg/system"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

const (
	// ImageLayoutVersion is the version of the image layout we support. This
	// value is *not* the same as imagespec.Version, and the meaning of this
	// field is still under discussion in the spec. For now we'll just hardcode
	// the value and hope for the best.
	ImageLayoutVersion = "1.0.0"

	// refDirectory is the directory inside an OCI image that contains references.
	refDirectory = "refs"

	// blobDirectory is the directory inside an OCI image that contains blobs.
	blobDirectory = "blobs"

	// layoutFile is the file in side an OCI image the indicates what version
	// of the OCI spec the image is.
	layoutFile = "oci-layout"
)

// blobPath returns the path to a blob given its digest, relative to the root
// of the OCI image. The digest must be of the form algorithm:hex.
func blobPath(digest digest.Digest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", errors.Wrapf(err, "invalid digest: %q", digest)
	}

	algo := digest.Algorithm()
	hash := digest.Hex()

	if algo != cas.BlobAlgorithm {
		return "", errors.Errorf("unsupported algorithm: %q", algo)
	}

	return filepath.Join(blobDirectory, algo.String(), hash), nil
}

// refPath returns the path to a reference given its name, relative to the r
// oot of the OCI image.
func refPath(name string) (string, error) {
	return filepath.Join(refDirectory, name), nil
}

type dirEngine struct {
	path     string
	temp     string
	tempFile *os.File
}

func (e *dirEngine) ensureTempDir() error {
	if e.temp == "" {
		tempDir, err := ioutil.TempDir(e.path, "tmp-")
		if err != nil {
			return errors.Wrap(err, "create tempdir")
		}

		// We get an advisory lock to ensure that GC() won't delete our
		// temporary directory here. Once we get the lock we know it won't do
		// anything until we unlock it or exit.

		e.tempFile, err = os.Open(tempDir)
		if err != nil {
			return errors.Wrap(err, "open tempdir for lock")
		}
		if err := system.Flock(e.tempFile.Fd(), true); err != nil {
			return errors.Wrap(err, "lock tempdir")
		}

		e.temp = tempDir
	}
	return nil
}

// verify ensures that the image is valid.
func (e *dirEngine) validate() error {
	content, err := ioutil.ReadFile(filepath.Join(e.path, layoutFile))
	if err != nil {
		if os.IsNotExist(err) {
			err = cas.ErrInvalid
		}
		return errors.Wrap(err, "read oci-layout")
	}

	var ociLayout ispec.ImageLayout
	if err := json.Unmarshal(content, &ociLayout); err != nil {
		return errors.Wrap(err, "parse oci-layout")
	}

	// XXX: Currently the meaning of this field is not adequately defined by
	//      the spec, nor is the "official" value determined by the spec.
	if ociLayout.Version != ImageLayoutVersion {
		return errors.Wrap(cas.ErrInvalid, "layout version is supported")
	}

	// Check that "blobs" and "refs" exist in the image.
	// FIXME: We also should check that blobs *only* contains a cas.BlobAlgorithm
	//        directory (with no subdirectories) and that refs *only* contains
	//        files (optionally also making sure they're all JSON descriptors).
	if fi, err := os.Stat(filepath.Join(e.path, blobDirectory)); err != nil {
		if os.IsNotExist(err) {
			err = cas.ErrInvalid
		}
		return errors.Wrap(err, "check blobdir")
	} else if !fi.IsDir() {
		return errors.Wrap(cas.ErrInvalid, "blobdir is directory")
	}

	if fi, err := os.Stat(filepath.Join(e.path, refDirectory)); err != nil {
		if os.IsNotExist(err) {
			err = cas.ErrInvalid
		}
		return errors.Wrap(err, "check refdir")
	} else if !fi.IsDir() {
		return errors.Wrap(cas.ErrInvalid, "refdir is directory")
	}

	return nil
}

// PutBlob adds a new blob to the image. This is idempotent; a nil error
// means that "the content is stored at DIGEST" without implying "because
// of this PutBlob() call".
func (e *dirEngine) PutBlob(ctx context.Context, reader io.Reader) (digest.Digest, int64, error) {
	if err := e.ensureTempDir(); err != nil {
		return "", -1, errors.Wrap(err, "ensure tempdir")
	}

	digester := cas.BlobAlgorithm.Digester()

	// We copy this into a temporary file because we need to get the blob hash,
	// but also to avoid half-writing an invalid blob.
	fh, err := ioutil.TempFile(e.temp, "blob-")
	if err != nil {
		return "", -1, errors.Wrap(err, "create temporary blob")
	}
	tempPath := fh.Name()
	defer fh.Close()

	writer := io.MultiWriter(fh, digester.Hash())
	size, err := io.Copy(writer, reader)
	if err != nil {
		return "", -1, errors.Wrap(err, "copy to temporary blob")
	}
	fh.Close()

	// Get the digest.
	path, err := blobPath(digester.Digest())
	if err != nil {
		return "", -1, errors.Wrap(err, "compute blob name")
	}

	// Move the blob to its correct path.
	path = filepath.Join(e.path, path)
	if err := os.Rename(tempPath, path); err != nil {
		return "", -1, errors.Wrap(err, "rename temporary blob")
	}

	return digester.Digest(), int64(size), nil
}

// PutBlobJSON adds a new JSON blob to the image (marshalled from the given
// interface). This is equivalent to calling PutBlob() with a JSON payload
// as the reader. Note that due to intricacies in the Go JSON
// implementation, we cannot guarantee that two calls to PutBlobJSON() will
// return the same digest.
func (e *dirEngine) PutBlobJSON(ctx context.Context, data interface{}) (digest.Digest, int64, error) {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(data); err != nil {
		return "", -1, errors.Wrap(err, "encode JSON")
	}
	return e.PutBlob(ctx, &buffer)
}

// PutReference adds a new reference descriptor blob to the image. This is
// idempotent; a nil error means that "the descriptor is stored at NAME"
// without implying "because of this PutReference() call". ErrClobber is
// returned if there is already a descriptor stored at NAME, but does not
// match the descriptor requested to be stored.
func (e *dirEngine) PutReference(ctx context.Context, name string, descriptor ispec.Descriptor) error {
	if err := e.ensureTempDir(); err != nil {
		return errors.Wrap(err, "ensure tempdir")
	}

	if oldDescriptor, err := e.GetReference(ctx, name); err == nil {
		// We should not return an error if the two descriptors are identical.
		if !reflect.DeepEqual(oldDescriptor, descriptor) {
			return cas.ErrClobber
		}
		return nil
	} else if !os.IsNotExist(errors.Cause(err)) {
		return errors.Wrap(err, "get old reference")
	}

	// We copy this into a temporary file to avoid half-writing an invalid
	// reference.
	fh, err := ioutil.TempFile(e.temp, "ref."+name+"-")
	if err != nil {
		return errors.Wrap(err, "create temporary ref")
	}
	tempPath := fh.Name()
	defer fh.Close()

	// Write out descriptor.
	if err := json.NewEncoder(fh).Encode(descriptor); err != nil {
		return errors.Wrap(err, "encode temporary ref")
	}
	fh.Close()

	path, err := refPath(name)
	if err != nil {
		return errors.Wrap(err, "compute ref path")
	}

	// Move the ref to its correct path.
	path = filepath.Join(e.path, path)
	if err := os.Rename(tempPath, path); err != nil {
		return errors.Wrap(err, "rename temporary ref")
	}

	return nil
}

// GetBlob returns a reader for retrieving a blob from the image, which the
// caller must Close(). Returns os.ErrNotExist if the digest is not found.
func (e *dirEngine) GetBlob(ctx context.Context, digest digest.Digest) (io.ReadCloser, error) {
	path, err := blobPath(digest)
	if err != nil {
		return nil, errors.Wrap(err, "compute blob path")
	}
	fh, err := os.Open(filepath.Join(e.path, path))
	return fh, errors.Wrap(err, "open blob")
}

// GetReference returns a reference from the image. Returns os.ErrNotExist
// if the name was not found.
func (e *dirEngine) GetReference(ctx context.Context, name string) (ispec.Descriptor, error) {
	path, err := refPath(name)
	if err != nil {
		return ispec.Descriptor{}, errors.Wrap(err, "compute ref path")
	}

	content, err := ioutil.ReadFile(filepath.Join(e.path, path))
	if err != nil {
		return ispec.Descriptor{}, errors.Wrap(err, "read ref")
	}

	var descriptor ispec.Descriptor
	if err := json.Unmarshal(content, &descriptor); err != nil {
		return ispec.Descriptor{}, errors.Wrap(err, "parse ref")
	}

	// XXX: Do we need to validate the descriptor?
	return descriptor, nil
}

// DeleteBlob removes a blob from the image. This is idempotent; a nil
// error means "the content is not in the store" without implying "because
// of this DeleteBlob() call".
func (e *dirEngine) DeleteBlob(ctx context.Context, digest digest.Digest) error {
	path, err := blobPath(digest)
	if err != nil {
		return errors.Wrap(err, "compute blob path")
	}

	err = os.Remove(filepath.Join(e.path, path))
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "remove blob")
	}
	return nil
}

// DeleteReference removes a reference from the image. This is idempotent;
// a nil error means "the content is not in the store" without implying
// "because of this DeleteReference() call".
func (e *dirEngine) DeleteReference(ctx context.Context, name string) error {
	path, err := refPath(name)
	if err != nil {
		return errors.Wrap(err, "compute ref path")
	}

	err = os.Remove(filepath.Join(e.path, path))
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "remove ref")
	}
	return nil
}

// ListBlobs returns the set of blob digests stored in the image.
func (e *dirEngine) ListBlobs(ctx context.Context) ([]digest.Digest, error) {
	digests := []digest.Digest{}
	blobDir := filepath.Join(e.path, blobDirectory, cas.BlobAlgorithm.String())

	if err := filepath.Walk(blobDir, func(path string, _ os.FileInfo, _ error) error {
		// Skip the actual directory.
		if path == blobDir {
			return nil
		}

		// XXX: Do we need to handle multiple-directory-deep cases?
		digest := digest.NewDigestFromHex(cas.BlobAlgorithm.String(), filepath.Base(path))
		digests = append(digests, digest)
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "walk blobdir")
	}

	return digests, nil
}

// ListReferences returns the set of reference names stored in the image.
func (e *dirEngine) ListReferences(ctx context.Context) ([]string, error) {
	refs := []string{}
	refDir := filepath.Join(e.path, refDirectory)

	if err := filepath.Walk(refDir, func(path string, _ os.FileInfo, _ error) error {
		// Skip the actual directory.
		if path == refDir {
			return nil
		}

		// XXX: Do we need to handle multiple-directory-deep cases?
		refs = append(refs, filepath.Base(path))
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "walk refdir")
	}

	return refs, nil
}

// Clean executes a garbage collection of any non-blob garbage in the store
// (this includes temporary files and directories not reachable from the CAS
// interface). This MUST NOT remove any blobs or references in the store.
func (e *dirEngine) Clean(ctx context.Context) error {
	// Effectively we are going to remove every directory except the standard
	// directories, unless they have a lock already.
	fh, err := os.Open(e.path)
	if err != nil {
		return errors.Wrap(err, "open imagedir")
	}
	defer fh.Close()

	children, err := fh.Readdir(-1)
	if err != nil {
		return errors.Wrap(err, "readdir imagedir")
	}

	for _, child := range children {
		// Skip any children that are expected to exist.
		switch child.Name() {
		case blobDirectory, refDirectory, layoutFile:
			continue
		}

		// Try to get a lock on the directory.
		path := filepath.Join(e.path, child.Name())
		cfh, err := os.Open(path)
		if err != nil {
			// Ignore errors because it might've been deleted underneath us.
			continue
		}
		defer cfh.Close()

		if err := system.Flock(cfh.Fd(), true); err != nil {
			// If we fail to get a flock(2) then it's probably already locked,
			// so we shouldn't touch it.
			continue
		}
		defer system.Unflock(cfh.Fd())

		if err := os.RemoveAll(path); err != nil {
			return errors.Wrap(err, "remove garbage path")
		}
	}

	return nil
}

// Close releases all references held by the e. Subsequent operations may
// fail.
func (e *dirEngine) Close() error {
	if e.temp != "" {
		if err := system.Unflock(e.tempFile.Fd()); err != nil {
			return errors.Wrap(err, "unlock tempdir")
		}
		if err := e.tempFile.Close(); err != nil {
			return errors.Wrap(err, "close tempdir")
		}
		if err := os.RemoveAll(e.temp); err != nil {
			return errors.Wrap(err, "remove tempdir")
		}
	}
	return nil
}

// Open opens a new reference to the directory-backed OCI image referenced by
// the provided path.
func Open(path string) (cas.Engine, error) {
	engine := &dirEngine{
		path: path,
		temp: "",
	}

	if err := engine.validate(); err != nil {
		return nil, errors.Wrap(err, "validate")
	}

	return engine, nil
}

// Create creates a new OCI image layout at the given path. If the path already
// exists, os.ErrExist is returned. However, all of the parent components of
// the path will be created if necessary.
func Create(path string) error {
	// We need to fail if path already exists, but we first create all of the
	// parent paths.
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errors.Wrap(err, "mkdir parent")
		}
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return errors.Wrap(err, "mkdir")
	}

	// Create the necessary directories and "oci-layout" file.
	if err := os.Mkdir(filepath.Join(path, blobDirectory), 0755); err != nil {
		return errors.Wrap(err, "mkdir blobdir")
	}
	if err := os.Mkdir(filepath.Join(path, blobDirectory, cas.BlobAlgorithm.String()), 0755); err != nil {
		return errors.Wrap(err, "mkdir algorithm")
	}
	if err := os.Mkdir(filepath.Join(path, refDirectory), 0755); err != nil {
		return errors.Wrap(err, "mkdir refdir")
	}

	fh, err := os.Create(filepath.Join(path, layoutFile))
	if err != nil {
		return errors.Wrap(err, "create oci-layout")
	}
	defer fh.Close()

	ociLayout := &ispec.ImageLayout{
		Version: ImageLayoutVersion,
	}

	if err := json.NewEncoder(fh).Encode(ociLayout); err != nil {
		return errors.Wrap(err, "encode oci-layout")
	}

	// Everything is now set up.
	return nil
}
