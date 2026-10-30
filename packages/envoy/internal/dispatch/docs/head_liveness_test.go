package docs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The liveness ping (headLiveness) keeps a healthy idle LISTEN connection delivering while its
// process holds something the head tracker's trust serves, and costs nothing otherwise.

// The evictor's liveness ping keeps the head tracker trusting a healthy LISTEN connection, and the
// tracker's trust protects only what this process answers without the database: a resident room,
// a cached cold read, a preload. A process holding none of those has nothing the ping protects, so
// it sends none. On a real Dispatch pair left idle with no room, no cached read and no tracker
// entry, the two processes sent 118 pings a minute (240 with four processes), where the build
// before the ping sent none.
func TestAServiceWithNothingTheTrackerProtectsSendsNoPing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	listen, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the ping listener: %v", err)
	}
	t.Cleanup(func() { _ = listen.Close(context.Background()) })
	if _, err := listen.Exec(ctx, "listen "+documentPingChannel); err != nil {
		t.Fatalf("listen on %s: %v", documentPingChannel, err)
	}
	runProbedEvictor(t, service, nil)
	if live := len(service.srv.Rooms()); live != 0 {
		t.Fatalf("%d rooms are live, so the service holds something and nothing here is tested", live)
	}

	window := headTrustWindow + 2*headPingAfter
	deadline := time.Now().Add(window)
	pings := 0
	for {
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		_, err := listen.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			break
		}
		pings++
	}
	if figures := service.RoomFigures(); figures.ResidentRooms != 0 || figures.ReadCache.Entries != 0 || figures.Preloads != 0 || figures.HeadTracker.Entries != 0 {
		t.Fatalf("the service came to hold something during the window (%+v), so nothing here is tested", figures)
	}
	if pings != 0 {
		t.Errorf("a service with no room, no cached read, no preload and no tracker entry sent %d pings on %s in %s, want none: the ping protects nothing there and costs a shared-pool connection and a NOTIFY to every listener each second", pings, documentPingChannel, window)
	}
}

// A tracker entry that only records another process's write - a document this process never read,
// holds no room for, caches nothing of and has no preload of - protects nothing, so hearing that
// write does not start this process pinging. Every process hears every write in the deployment,
// and an entry stays until the LISTEN reconnects, so counting such entries as something held would
// have every process ping for good after the first write anywhere. On a real Dispatch pair, one
// REST edit on the first process left both pinging 47 times a minute two minutes later, each with
// no room and no cached read and one tracker entry.
func TestHearingAnotherProcesssWriteAloneStartsNoPing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	writer, _ := newTestServiceInstance(t, database)
	listenerStore := storetest.SecondProcess(t, database)
	listener := newReadCacheService(t, listenerStore, NewPgVersioned(listenerStore))
	listen, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the ping listener: %v", err)
	}
	t.Cleanup(func() { _ = listen.Close(context.Background()) })
	if _, err := listen.Exec(ctx, "listen "+documentPingChannel); err != nil {
		t.Fatalf("listen on %s: %v", documentPingChannel, err)
	}
	probe := runProbedEvictor(t, listener, nil)
	seedServiceText(t, writer, artifactID, "Write 0.\n")
	// The listening service holds nothing, so its tracker keeps no entry for the write it hears;
	// the evictor deciding a notification sent after the write proves the write's notice arrived.
	probe.awaitDecided(t, database)

	window := headTrustWindow + 2*headPingAfter
	deadline := time.Now().Add(window)
	pings := 0
	for {
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		notification, err := listen.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			break
		}
		if notification.Payload == listener.ProcessID() {
			pings++
		}
	}
	if figures := listener.RoomFigures(); figures.ResidentRooms != 0 || figures.ReadCache.Entries != 0 || figures.Preloads != 0 {
		t.Fatalf("the listening service came to hold a room, a cached read or a preload (%+v), so nothing here is tested", figures)
	}
	if pings != 0 {
		t.Errorf("a service that only heard another service's write - no room, no cached read, no preload - sent %d pings on %s in %s, want none: a tracker entry for a document it never read protects nothing", pings, documentPingChannel, window)
	}
}

// Four processes on one database, each holding a cached cold read with its evictor running, send
// about one ping a second between them, not one each: every ping reaches every listener and draws
// each one's quiet window afresh, and the jitter keeps the processes that heard it from pinging
// together. On a real deployment of four processes each pinged once a second, 240 pings a minute,
// where the claim was one ping a second for the whole deployment. Every tracker still trusts
// itself at the end, so the fewer pings still keep each connection proving it delivers.
func TestFourServicesHoldingSomethingSendAboutOnePingASecondBetweenThem(t *testing.T) {
	const (
		processes = 4
		window    = 12 * time.Second
		perSecond = 1.5
	)
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	services := make([]*Service, processes)
	for i := range services {
		processStore := database
		if i > 0 {
			processStore = storetest.SecondProcess(t, database)
		}
		services[i] = newReadCacheService(t, processStore, NewPgVersioned(processStore))
	}
	seedServiceText(t, services[0], artifactID, "Seeded.\n")
	listen, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the ping listener: %v", err)
	}
	t.Cleanup(func() { _ = listen.Close(context.Background()) })
	if _, err := listen.Exec(ctx, "listen "+documentPingChannel); err != nil {
		t.Fatalf("listen on %s: %v", documentPingChannel, err)
	}
	for i, service := range services {
		runProbedEvictor(t, service, nil)
		if text, err := service.Text(ctx, artifactID); err != nil || text != "Seeded.\n" {
			t.Fatalf("service %d: cold read = %q (%v), want the seeded text", i, text, err)
		}
		if cacheEntry(t, service, artifactID) == nil {
			t.Fatalf("service %d cached no cold read, so it holds nothing the ping protects and nothing here is tested", i)
		}
	}

	deadline := time.Now().Add(window)
	senders := map[string]int{}
	pings := 0
	for {
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		notification, err := listen.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			break
		}
		pings++
		senders[notification.Payload]++
	}
	t.Logf("%d services each holding a cached cold read sent %d pings on %s in %s (%.2f a second), from %d of them", processes, pings, documentPingChannel, window, float64(pings)/window.Seconds(), len(senders))
	if limit := int(perSecond * window.Seconds()); pings > limit {
		t.Errorf("%d services sent %d pings in %s, want at most %d (%.1f a second for the whole deployment): every process that heard a ping pinged again together", processes, pings, window, limit, perSecond)
	}
	if pings == 0 {
		t.Errorf("%d services each holding a cached cold read sent no ping in %s, want about one a second: nothing keeps their trackers trusting an idle connection", processes, window)
	}
	for i, service := range services {
		if figures := service.RoomFigures(); !figures.HeadTracker.Trusted || figures.ReadCache.Entries == 0 {
			t.Errorf("service %d at the end of the window: head tracker %+v, read cache %+v, want trusted and still holding its cached read", i, figures.HeadTracker, figures.ReadCache)
		}
	}
}
