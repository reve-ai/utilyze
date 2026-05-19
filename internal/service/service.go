package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/systalyze/utilyze/internal/metrics"
)

var disableSampler = os.Getenv("UTLZ_DISABLE_SAMPLER") == "1"

const (
	DefaultHost = "127.0.0.1"
	DefaultPort = "8079"
	DefaultAddr = DefaultHost + ":" + DefaultPort
	LivePath    = "/live"
)

type EventType string

const (
	EventInit     EventType = "init"
	EventMetrics  EventType = "metrics"
	EventCeilings EventType = "ceilings"
)

type Event struct {
	Type      EventType                  `json:"type"`
	DeviceIDs []int                      `json:"deviceIds,omitempty"`
	Snapshot  *metrics.MetricsSnapshot   `json:"snapshot,omitempty"`
	Ceilings  map[int]metrics.GpuCeiling `json:"ceilings,omitempty"`
}

type Service struct {
	mux      *http.ServeMux
	mu       sync.Mutex
	clients  map[chan Event]string // event chan -> client ID
	lastInit *Event
}

func NewService() *Service {
	s := &Service{
		mux:     http.NewServeMux(),
		clients: make(map[chan Event]string),
	}
	s.mux.HandleFunc(LivePath, s.handleLive)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Service) Run(ctx context.Context, addr string) error {
	if addr == "" {
		addr = DefaultAddr
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	server := &http.Server{
		Addr:    addr,
		Handler: s,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Service) RunCollector(ctx context.Context, collector *metrics.Collector, observe func(metrics.MetricsSnapshot)) {
	s.SetDeviceIDs(collector.MonitoredDeviceIDs())

	snapshots := make(chan metrics.MetricsSnapshot)
	if disableSampler {
		slog.Info("UTLZ_DISABLE_SAMPLER set")
		close(snapshots)
		return
	}
	go collector.Start(ctx, snapshots)

	for snapshot := range snapshots {
		if observe != nil {
			observe(snapshot)
		}
		s.BroadcastSnapshot(snapshot)
	}
}

func (s *Service) SetDeviceIDs(deviceIDs []int) {
	event := Event{
		Type:      EventInit,
		DeviceIDs: slices.Clone(deviceIDs),
	}

	s.mu.Lock()
	s.lastInit = &event
	s.broadcastLocked(event)
	s.mu.Unlock()
}

func (s *Service) BroadcastSnapshot(snapshot metrics.MetricsSnapshot) {
	s.Broadcast(Event{Type: EventMetrics, Snapshot: &snapshot})
}

func (s *Service) BroadcastCeilings(ceilings map[int]metrics.GpuCeiling) {
	s.Broadcast(Event{Type: EventCeilings, Ceilings: maps.Clone(ceilings)})
}

func (s *Service) Broadcast(event Event) {
	s.mu.Lock()
	s.broadcastLocked(event)
	s.mu.Unlock()
}

func (s *Service) ConnectedClientIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]struct{}, len(s.clients))
	for _, clientID := range s.clients {
		if clientID != "" {
			seen[clientID] = struct{}{}
		}
	}

	clientIDs := slices.Collect(maps.Keys(seen))
	sort.Strings(clientIDs)
	return clientIDs
}

func (s *Service) broadcastLocked(event Event) {
	for client := range s.clients {
		select {
		case client <- event:
		default:
		}
	}
}

func (s *Service) handleLive(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(context.Background())
	client := make(chan Event, 32)
	lastInit := s.addClient(client, clientID)
	defer s.removeClient(client)

	if lastInit != nil {
		if err := writeEvent(ctx, conn, *lastInit); err != nil {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-client:
			if err := writeEvent(ctx, conn, event); err != nil {
				return
			}
		}
	}
}

func (s *Service) addClient(client chan Event, clientID string) *Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clients[client] = clientID
	if s.lastInit == nil {
		return nil
	}
	event := *s.lastInit
	event.DeviceIDs = slices.Clone(event.DeviceIDs)
	return &event
}

func (s *Service) removeClient(client chan Event) {
	s.mu.Lock()
	delete(s.clients, client)
	s.mu.Unlock()
}

func writeEvent(ctx context.Context, conn *websocket.Conn, event Event) error {
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(writeCtx, conn, event)
}
