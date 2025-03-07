package pubsubmon

import (
	"context"
	"fmt"

	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/ipfs-cluster/api"
	"github.com/ipfs/ipfs-cluster/test"

	libp2p "github.com/libp2p/go-libp2p"
	host "github.com/libp2p/go-libp2p-core/host"
	peer "github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

func init() {
	// GossipSub needs to heartbeat to discover newly connected hosts
	// This speeds things up a little.
	pubsub.GossipSubHeartbeatInterval = 50 * time.Millisecond
}

type metricFactory struct {
	l       sync.Mutex
	counter int
}

func newMetricFactory() *metricFactory {
	return &metricFactory{
		counter: 0,
	}
}

func (mf *metricFactory) newMetric(n string, p peer.ID) *api.Metric {
	mf.l.Lock()
	defer mf.l.Unlock()
	m := &api.Metric{
		Name:  n,
		Peer:  p,
		Value: fmt.Sprintf("%d", mf.counter),
		Valid: true,
	}
	m.SetTTL(5 * time.Second)
	mf.counter++
	return m
}

func (mf *metricFactory) count() int {
	mf.l.Lock()
	defer mf.l.Unlock()
	return mf.counter
}

func peers(ctx context.Context) ([]peer.ID, error) {
	return []peer.ID{test.PeerID1, test.PeerID2, test.PeerID3}, nil
}

func testPeerMonitor(t *testing.T) (*Monitor, host.Host, func()) {
	ctx := context.Background()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
	)
	if err != nil {
		t.Fatal(err)
	}

	psub, err := pubsub.NewGossipSub(
		ctx,
		h,
		pubsub.WithMessageSigning(true),
		pubsub.WithStrictSignatureVerification(true),
	)
	if err != nil {
		h.Close()
		t.Fatal(err)
	}

	mock := test.NewMockRPCClientWithHost(t, h)
	cfg := &Config{}
	cfg.Default()
	cfg.CheckInterval = 2 * time.Second
	mon, err := New(ctx, cfg, psub, peers)
	if err != nil {
		t.Fatal(err)
	}
	mon.SetClient(mock)

	shutdownF := func() {
		mon.Shutdown(ctx)
		h.Close()
	}

	return mon, h, shutdownF
}

func TestPeerMonitorShutdown(t *testing.T) {
	ctx := context.Background()
	pm, _, shutdown := testPeerMonitor(t)
	defer shutdown()

	err := pm.Shutdown(ctx)
	if err != nil {
		t.Error(err)
	}

	err = pm.Shutdown(ctx)
	if err != nil {
		t.Error(err)
	}
}

func TestLogMetricConcurrent(t *testing.T) {
	ctx := context.Background()
	pm, _, shutdown := testPeerMonitor(t)
	defer shutdown()

	var wg sync.WaitGroup
	wg.Add(3)

	// Insert 25 metrics
	f := func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			mt := &api.Metric{
				Name:  "test",
				Peer:  test.PeerID1,
				Value: fmt.Sprintf("%d", time.Now().UnixNano()),
				Valid: true,
			}
			mt.SetTTL(150 * time.Millisecond)
			pm.LogMetric(ctx, mt)
			time.Sleep(75 * time.Millisecond)
		}
	}
	go f()
	go f()
	go f()

	// Wait for at least two metrics to be inserted
	time.Sleep(200 * time.Millisecond)
	last := time.Now().Add(-500 * time.Millisecond)

	for i := 0; i <= 20; i++ {
		lastMtrcs := pm.LatestMetrics(ctx, "test")

		// There should always 1 valid LatestMetric "test"
		if len(lastMtrcs) != 1 {
			t.Error("no valid metrics", len(lastMtrcs), i)
			time.Sleep(75 * time.Millisecond)
			continue
		}

		n, err := strconv.Atoi(lastMtrcs[0].Value)
		if err != nil {
			t.Fatal(err)
		}

		// The timestamp of the metric cannot be older than
		// the timestamp from the last
		current := time.Unix(0, int64(n))
		if current.Before(last) {
			t.Errorf("expected newer metric: Current: %s, Last: %s", current, last)
		}
		last = current
		time.Sleep(75 * time.Millisecond)
	}

	wg.Wait()
}

func TestPeerMonitorLogMetric(t *testing.T) {
	ctx := context.Background()
	pm, _, shutdown := testPeerMonitor(t)
	defer shutdown()
	mf := newMetricFactory()

	// dont fill window
	pm.LogMetric(ctx, mf.newMetric("test", test.PeerID1))
	pm.LogMetric(ctx, mf.newMetric("test", test.PeerID2))
	pm.LogMetric(ctx, mf.newMetric("test", test.PeerID3))

	// fill window
	pm.LogMetric(ctx, mf.newMetric("test2", test.PeerID3))
	pm.LogMetric(ctx, mf.newMetric("test2", test.PeerID3))
	pm.LogMetric(ctx, mf.newMetric("test2", test.PeerID3))
	pm.LogMetric(ctx, mf.newMetric("test2", test.PeerID3))

	latestMetrics := pm.LatestMetrics(ctx, "testbad")
	if len(latestMetrics) != 0 {
		t.Logf("%+v", latestMetrics)
		t.Error("metrics should be empty")
	}

	latestMetrics = pm.LatestMetrics(ctx, "test")
	if len(latestMetrics) != 3 {
		t.Error("metrics should correspond to 3 hosts")
	}

	for _, v := range latestMetrics {
		switch v.Peer {
		case test.PeerID1:
			if v.Value != "0" {
				t.Error("bad metric value")
			}
		case test.PeerID2:
			if v.Value != "1" {
				t.Error("bad metric value")
			}
		case test.PeerID3:
			if v.Value != "2" {
				t.Error("bad metric value")
			}
		default:
			t.Error("bad peer")
		}
	}

	latestMetrics = pm.LatestMetrics(ctx, "test2")
	if len(latestMetrics) != 1 {
		t.Fatal("should only be one metric")
	}
	if latestMetrics[0].Value != fmt.Sprintf("%d", mf.count()-1) {
		t.Error("metric is not last")
	}
}

func TestPeerMonitorPublishMetric(t *testing.T) {
	ctx := context.Background()
	pm, host, shutdown := testPeerMonitor(t)
	defer shutdown()

	pm2, host2, shutdown2 := testPeerMonitor(t)
	defer shutdown2()

	time.Sleep(200 * time.Millisecond)

	err := host.Connect(
		context.Background(),
		peer.AddrInfo{
			ID:    host2.ID(),
			Addrs: host2.Addrs(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	mf := newMetricFactory()

	metric := mf.newMetric("test", test.PeerID1)
	err = pm.PublishMetric(ctx, metric)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	checkMetric := func(t *testing.T, pm *Monitor) {
		latestMetrics := pm.LatestMetrics(ctx, "test")
		if len(latestMetrics) != 1 {
			t.Fatal(host.ID(), "expected 1 published metric")
		}
		t.Log(host.ID(), "received metric")

		receivedMetric := latestMetrics[0]
		if receivedMetric.Peer != metric.Peer ||
			receivedMetric.Expire != metric.Expire ||
			receivedMetric.Value != metric.Value ||
			receivedMetric.Valid != metric.Valid ||
			receivedMetric.Name != metric.Name {
			t.Fatal("it should be exactly the same metric we published")
		}
	}

	t.Log("pm1")
	checkMetric(t, pm)
	t.Log("pm2")
	checkMetric(t, pm2)
}

func TestPeerMonitorAlerts(t *testing.T) {
	ctx := context.Background()
	pm, _, shutdown := testPeerMonitor(t)
	defer shutdown()
	mf := newMetricFactory()

	mtr := mf.newMetric("test", test.PeerID1)
	mtr.SetTTL(0)
	pm.LogMetric(ctx, mtr)
	time.Sleep(time.Second)
	timeout := time.NewTimer(time.Second * 5)

	// it should alert once.
	for i := 0; i < 1; i++ {
		select {
		case <-timeout.C:
			t.Fatal("should have thrown an alert by now")
		case alrt := <-pm.Alerts():
			if alrt.Name != "test" {
				t.Error("Alert should be for test")
			}
			if alrt.Peer != test.PeerID1 {
				t.Error("Peer should be TestPeerID1")
			}
		}
	}
}

func TestMetricsGetsDeleted(t *testing.T) {
	ctx := context.Background()

	pm, _, shutdown := testPeerMonitor(t)
	defer shutdown()
	mf := newMetricFactory()

	pm.LogMetric(ctx, mf.newMetric("test", test.PeerID1))
	metrics := pm.metrics.PeerMetrics(test.PeerID1)
	if len(metrics) == 0 {
		t.Error("expected metrics")
	}

	// TODO: expiry time + checkInterval is 7 sec
	// Why does it need 9 or more?
	time.Sleep(9 * time.Second)

	metrics = pm.metrics.PeerMetrics(test.PeerID1)
	if len(metrics) > 0 {
		t.Error("expected no metrics")
	}
}
