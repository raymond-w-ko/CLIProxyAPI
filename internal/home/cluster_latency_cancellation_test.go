package home

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClusterLatencyCancellationKeepsDispatchClient(t *testing.T) {
	for _, operation := range []string{"report", "probe"} {
		t.Run(operation, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			client, _ := newRedisCommandTestClient(t, func(args []string) string {
				isReport := len(args) > 2 && args[0] == "CLUSTER" && args[2] == "LATENCY"
				isProbe := len(args) == 2 && args[0] == "PING" && strings.HasPrefix(args[1], "home-probe/")
				if (operation == "report" && isReport) || (operation == "probe" && isProbe) {
					once.Do(func() { close(blocked) })
					<-release
				}
				return balanceTestRedisReply(args)
			})
			t.Cleanup(func() { close(release) })
			enableBalanceTestClient(client)
			client.balance.latencySupported = true
			dispatchClient := client.cmd
			if errPing := client.Ping(context.Background()); errPing != nil {
				t.Fatal(errPing)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "report" {
					done <- client.reportClusterLatency(ctx, &clusterLatencyHistory{}, time.Now())
				} else {
					_, errProbe := client.probeClusterLatency(ctx, clusterNodeAddress(clusterNode{IP: client.homeCfg.Host, Port: client.homeCfg.Port}))
					done <- errProbe
				}
			}()
			select {
			case <-blocked:
			case errCall := <-done:
				t.Fatalf("telemetry ended before blocked command: %v", errCall)
			case <-time.After(3 * time.Second):
				t.Fatal("telemetry did not reach blocked command")
			}
			cancel()
			select {
			case errCall := <-done:
				if errCall == nil {
					t.Fatal("canceled telemetry succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("lifetime cancellation did not interrupt telemetry")
			}
			if client.cmd != dispatchClient {
				t.Fatal("telemetry replaced the dispatch client")
			}
			if errPing := client.Ping(context.Background()); errPing != nil {
				t.Fatalf("telemetry cancellation closed the dispatch client: %v", errPing)
			}
		})
	}
}
