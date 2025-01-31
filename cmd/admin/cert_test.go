package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmhodges/clock"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/letsencrypt/boulder/core"
	corepb "github.com/letsencrypt/boulder/core/proto"
	berrors "github.com/letsencrypt/boulder/errors"
	blog "github.com/letsencrypt/boulder/log"
	rapb "github.com/letsencrypt/boulder/ra/proto"
	"github.com/letsencrypt/boulder/revocation"
	"github.com/letsencrypt/boulder/sa"
	sapb "github.com/letsencrypt/boulder/sa/proto"
	"github.com/letsencrypt/boulder/test"
	"github.com/letsencrypt/boulder/test/vars"
)

// mockSAWithIncident is a mock which only implements the SerialsForIncident
// gRPC method. It can be initialized with a set of serials for that method
// to return.
type mockSAWithIncident struct {
	sapb.StorageAuthorityReadOnlyClient
	incidentSerials []string
}

// SerialsForIncident returns a fake gRPC stream client object which itself
// will return the mockSAWithIncident's serials in order.
func (msa *mockSAWithIncident) SerialsForIncident(_ context.Context, _ *sapb.SerialsForIncidentRequest, _ ...grpc.CallOption) (sapb.StorageAuthorityReadOnly_SerialsForIncidentClient, error) {
	return &mockSerialsForIncidentClient{unsentSerials: msa.incidentSerials}, nil
}

type mockSerialsForIncidentClient struct {
	grpc.ClientStream
	unsentSerials []string
}

// Recv returns the next serial from the pre-loaded list.
func (c *mockSerialsForIncidentClient) Recv() (*sapb.IncidentSerial, error) {
	if len(c.unsentSerials) > 0 {
		res := c.unsentSerials[0]
		c.unsentSerials = c.unsentSerials[1:]
		return &sapb.IncidentSerial{Serial: res}, nil
	}
	return nil, io.EOF
}

func TestSerialsFromIncidentTable(t *testing.T) {
	t.Parallel()
	serials := []string{"foo", "bar", "baz"}

	a := admin{
		saroc: &mockSAWithIncident{incidentSerials: serials},
	}

	res, err := a.serialsFromIncidentTable(context.Background(), "tablename")
	test.AssertNotError(t, err, "getting serials from mock SA")
	test.AssertDeepEquals(t, res, serials)
}

func TestSerialsFromFile(t *testing.T) {
	t.Parallel()
	serials := []string{"foo", "bar", "baz"}

	serialsFile := path.Join(t.TempDir(), "serials.txt")
	err := os.WriteFile(serialsFile, []byte(strings.Join(serials, "\n")), os.ModeAppend)
	test.AssertNotError(t, err, "writing temp serials file")

	a := admin{}

	res, err := a.serialsFromFile(context.Background(), serialsFile)
	test.AssertNotError(t, err, "getting serials from file")
	test.AssertDeepEquals(t, res, serials)
}

func TestSerialsFromPrivateKey(t *testing.T) {
	serials := []string{"foo", "bar", "baz"}
	fc := clock.NewFake()
	fc.Set(time.Now())

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.AssertNotError(t, err, "creating test private key")
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	test.AssertNotError(t, err, "marshalling test private key bytes")

	keyFile := path.Join(t.TempDir(), "key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes})
	err = os.WriteFile(keyFile, keyPEM, os.ModeAppend)
	test.AssertNotError(t, err, "writing test private key file")

	keyHash, err := core.KeyDigest(privKey.Public())
	test.AssertNotError(t, err, "computing test SPKI hash")

	dbMap, err := sa.DBMapForTest(vars.DBConnSA)
	test.AssertNotError(t, err, "creating test dbMap")
	defer test.ResetBoulderTestDatabase(t)

	for _, serial := range serials {
		_, err = dbMap.ExecContext(
			context.Background(),
			"INSERT INTO keyHashToSerial(keyHash, certNotAfter, certSerial) VALUES (?, ?, ?)",
			keyHash[:],
			fc.Now().Add(24*time.Hour),
			serial,
		)
		test.AssertNotError(t, err, "inserting fake serial into test db")
	}

	a := admin{dbMap: dbMap}

	res, err := a.serialsFromPrivateKey(context.Background(), keyFile)
	test.AssertNotError(t, err, "getting serials from keyHashToSerial table")
	test.AssertDeepEquals(t, res, serials)
}

// mockSAWithRegistration is a mock which only implements the GetRegistration
// gRPC method. It can be initialized with a regID to recognize.
type mockSAWithRegistration struct {
	sapb.StorageAuthorityReadOnlyClient
	regID int64
}

// GetRegistration is a mock which only returns a valid registration object if
// it recognizes the regID in the request.
func (msa *mockSAWithRegistration) GetRegistration(ctx context.Context, req *sapb.RegistrationID, _ ...grpc.CallOption) (*corepb.Registration, error) {
	if req.Id == msa.regID {
		return &corepb.Registration{}, nil
	}
	return nil, errors.New("no such reg")
}

func TestSerialsFromRegID(t *testing.T) {
	serials := []string{"foo", "bar", "baz"}
	fc := clock.NewFake()
	fc.Set(time.Now())

	dbMap, err := sa.DBMapForTest(vars.DBConnSA)
	test.AssertNotError(t, err, "creating test dbMap")
	defer test.ResetBoulderTestDatabase(t)

	_, err = dbMap.ExecContext(
		context.Background(),
		"INSERT INTO registrations(id, jwk, jwk_sha256, contact, agreement, LockCol, createdAt) VALUES (?, ?, ?, ?, ?, ?, ?)",
		123, "", "", "", "", 0, fc.Now().Add(-24*time.Hour),
	)
	test.AssertNotError(t, err, "inserting fake serial into test db")

	for _, serial := range serials {
		_, err = dbMap.ExecContext(
			context.Background(),
			"INSERT INTO serials(registrationID, serial, created, expires) VALUES (?, ?, ?, ?)",
			123,
			serial,
			fc.Now().Add(-24*time.Hour),
			fc.Now().Add(24*time.Hour),
		)
		test.AssertNotError(t, err, "inserting fake serial into test db")
	}

	a := admin{saroc: &mockSAWithRegistration{regID: 123}, dbMap: dbMap}

	res, err := a.serialsFromRegID(context.Background(), 123)
	test.AssertNotError(t, err, "getting serials from serials table")
	test.AssertDeepEquals(t, res, serials)
}

// mockRARecordingRevocations is a mock which only implements the
// AdministrativelyRevokeCertificate gRPC method. It can be initialized with
// serials to recognize as already revoked, or to fail.
type mockRARecordingRevocations struct {
	rapb.RegistrationAuthorityClient
	doomedToFail       []string
	alreadyRevoked     []string
	revocationRequests []*rapb.AdministrativelyRevokeCertificateRequest
	sync.Mutex
}

// AdministrativelyRevokeCertificate records the request it received on the mock
// RA struct, and succeeds if it doesn't recognize the serial as one it should
// fail for.
func (mra *mockRARecordingRevocations) AdministrativelyRevokeCertificate(_ context.Context, req *rapb.AdministrativelyRevokeCertificateRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	mra.Lock()
	defer mra.Unlock()
	mra.revocationRequests = append(mra.revocationRequests, req)
	if slices.Contains(mra.doomedToFail, req.Serial) {
		return nil, errors.New("oops")
	}
	if slices.Contains(mra.alreadyRevoked, req.Serial) {
		return nil, berrors.AlreadyRevokedError("too slow")
	}
	return &emptypb.Empty{}, nil
}

func (mra *mockRARecordingRevocations) reset() {
	mra.doomedToFail = nil
	mra.alreadyRevoked = nil
	mra.revocationRequests = nil
}

func TestRevokeSerials(t *testing.T) {
	t.Parallel()
	serials := []string{"foo", "bar", "baz"}

	mra := mockRARecordingRevocations{}
	log := blog.NewMock()
	a := admin{rac: &mra, log: log}

	assertRequestsContain := func(reqs []*rapb.AdministrativelyRevokeCertificateRequest, code revocation.Reason, skipBlockKey bool, malformed bool) {
		for _, req := range reqs {
			test.AssertEquals(t, len(req.Cert), 0)
			test.AssertEquals(t, req.Code, int64(code))
			test.AssertEquals(t, req.SkipBlockKey, skipBlockKey)
			test.AssertEquals(t, req.Malformed, malformed)
		}
	}

	// Revoking should result in 3 gRPC requests and quiet execution.
	mra.reset()
	log.Clear()
	a.dryRun = false
	err := a.revokeSerials(context.Background(), serials, 0, false, false, 1)
	test.AssertNotError(t, err, "")
	test.AssertEquals(t, len(log.GetAll()), 0)
	test.AssertEquals(t, len(mra.revocationRequests), 3)
	assertRequestsContain(mra.revocationRequests, 0, false, false)

	// Revoking an already-revoked serial should result in one log line.
	mra.reset()
	log.Clear()
	mra.alreadyRevoked = []string{"foo"}
	err = a.revokeSerials(context.Background(), serials, 0, false, false, 1)
	test.AssertNotError(t, err, "")
	test.AssertEquals(t, len(log.GetAllMatching("not revoking")), 1)
	test.AssertEquals(t, len(mra.revocationRequests), 3)
	assertRequestsContain(mra.revocationRequests, 0, false, false)

	// Revoking a doomed-to-fail serial should also result in one log line.
	mra.reset()
	log.Clear()
	mra.doomedToFail = []string{"bar"}
	err = a.revokeSerials(context.Background(), serials, 0, false, false, 1)
	test.AssertNotError(t, err, "")
	test.AssertEquals(t, len(log.GetAllMatching("failed to revoke")), 1)
	test.AssertEquals(t, len(mra.revocationRequests), 3)
	assertRequestsContain(mra.revocationRequests, 0, false, false)

	// Revoking with other parameters should get carried through.
	mra.reset()
	log.Clear()
	err = a.revokeSerials(context.Background(), serials, 1, true, true, 3)
	test.AssertNotError(t, err, "")
	test.AssertEquals(t, len(mra.revocationRequests), 3)
	assertRequestsContain(mra.revocationRequests, 1, true, true)

	// Revoking in dry-run mode should result in no gRPC requests and three logs.
	mra.reset()
	log.Clear()
	a.dryRun = true
	a.rac = dryRunRAC{log: log}
	err = a.revokeSerials(context.Background(), serials, 0, false, false, 1)
	test.AssertNotError(t, err, "")
	test.AssertEquals(t, len(log.GetAllMatching("dry-run:")), 3)
	test.AssertEquals(t, len(mra.revocationRequests), 0)
	assertRequestsContain(mra.revocationRequests, 0, false, false)
}
