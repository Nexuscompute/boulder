package notmain

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/rocsp"
	"github.com/letsencrypt/boulder/sa"
	"github.com/letsencrypt/boulder/test"
	"github.com/letsencrypt/boulder/test/vars"
	"golang.org/x/crypto/ocsp"
)

func makeClient() (*rocsp.WritingClient, clock.Clock) {
	CACertFile := "../../test/redis-tls/minica.pem"
	CertFile := "../../test/redis-tls/boulder/cert.pem"
	KeyFile := "../../test/redis-tls/boulder/key.pem"
	tlsConfig := cmd.TLSConfig{
		CACertFile: &CACertFile,
		CertFile:   &CertFile,
		KeyFile:    &KeyFile,
	}
	tlsConfig2, err := tlsConfig.Load()
	if err != nil {
		panic(err)
	}

	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:     []string{"10.33.33.2:4218"},
		Username:  "unittest-rw",
		Password:  "824968fa490f4ecec1e52d5e34916bdb60d45f8d",
		TLSConfig: tlsConfig2,
	})
	clk := clock.NewFake()
	return rocsp.NewWritingClient(rdb, 500*time.Millisecond, clk), clk
}

func TestGetStartingID(t *testing.T) {
	clk := clock.NewFake()
	dbMap, err := sa.NewDbMap(vars.DBConnSAFullPerms, sa.DbSettings{})
	test.AssertNotError(t, err, "failed setting up db client")
	defer test.ResetSATestDatabase(t)()
	sa.SetSQLDebug(dbMap, log.Get())

	cs := core.CertificateStatus{
		Serial:   "1337",
		NotAfter: clk.Now().Add(12 * time.Hour),
	}
	err = dbMap.Insert(&cs)
	test.AssertNotError(t, err, "inserting certificate status")
	firstID := cs.ID

	cs = core.CertificateStatus{
		Serial:   "1338",
		NotAfter: clk.Now().Add(36 * time.Hour),
	}
	err = dbMap.Insert(&cs)
	test.AssertNotError(t, err, "inserting certificate status")
	secondID := cs.ID
	t.Logf("first ID %d, second ID %d", firstID, secondID)

	clk.Sleep(48 * time.Hour)

	startingID, err := getStartingID(context.Background(), clk, dbMap.Db)
	test.AssertNotError(t, err, "getting starting ID")

	test.AssertEquals(t, startingID, secondID)
}

func TestStoreResponse(t *testing.T) {
	redisClient, clk := makeClient()

	issuer, err := core.LoadCert("../../test/hierarchy/int-e1.cert.pem")
	test.AssertNotError(t, err, "loading int-e1")

	issuerKey, err := test.LoadSigner("../../test/hierarchy/int-e1.key.pem")
	test.AssertNotError(t, err, "loading int-e1 key ")
	response, err := ocsp.CreateResponse(issuer, issuer, ocsp.Response{
		SerialNumber: big.NewInt(1337),
		Status:       0,
		ThisUpdate:   clk.Now(),
		NextUpdate:   clk.Now().Add(time.Hour),
	}, issuerKey)
	test.AssertNotError(t, err, "creating OCSP response")

	issuers, err := loadIssuers(map[string]int{
		"../../test/hierarchy/int-e1.cert.pem": 23,
	})
	if err != nil {
		t.Fatal(err)
	}

	cl := client{
		issuers:       issuers,
		redis:         redisClient,
		db:            nil,
		ocspGenerator: nil,
		clk:           clk,
	}

	ttl := time.Hour
	err = cl.storeResponse(context.Background(), response, &ttl)
	test.AssertNotError(t, err, "storing response")
}
