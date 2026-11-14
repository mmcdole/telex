package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"golang.org/x/time/rate"
)

var errNotAuthorized = errors.New("telegram session is not logged in: run `telex login` in a terminal")

// Telegram owns the single MTProto connection for the account.
type Telegram struct {
	cfg    Config
	log    *slog.Logger
	client *telegram.Client
	api    *tg.Client
	peers  *peers.Manager
	gaps   *updates.Manager
	waiter *floodwait.Waiter
	sender *message.Sender
	events *EventLog
	allow  *chatFilter

	mu    sync.Mutex
	ready chan struct{} // closed while connected and initialized
	dead  chan struct{} // closed once the session is known to be logged out
	self  *tg.User
	err   error // last fatal/connection error, reported to tool calls
}

// lockSession takes an exclusive lock so two processes never use the same
// auth key at once; Telegram answers that with AUTH_KEY_DUPLICATED and kills
// the session for both.
func lockSession(cfg Config) (*os.File, error) {
	f, err := os.OpenFile(cfg.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session %s is in use by another telex process (run one shared server with --http instead of one per client)", cfg.Home)
	}
	return f, nil
}

func deviceConfig() telegram.DeviceConfig {
	return telegram.DeviceConfig{
		DeviceModel:    "telex",
		SystemVersion:  runtime.GOOS + "/" + runtime.GOARCH,
		AppVersion:     version,
		SystemLangCode: "en",
		LangCode:       "en",
	}
}

func NewTelegram(cfg Config, store peers.Storage, events *EventLog, allow *chatFilter, log *slog.Logger) *Telegram {
	t := &Telegram{cfg: cfg, log: log, events: events, allow: allow, ready: make(chan struct{}), dead: make(chan struct{})}

	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		t.record(ctx, e, u.Message)
		return nil
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		t.record(ctx, e, u.Message)
		return nil
	})

	// The client needs the handler before peers/gaps exist (they need the
	// client's API), so route through a variable set just below.
	var handler telegram.UpdateHandler
	t.waiter = floodwait.NewWaiter().
		WithMaxWait(90 * time.Second). // longer waits come back to the agent as errors
		WithCallback(func(ctx context.Context, w floodwait.FloodWait) {
			log.Warn("flood wait", "duration", w.Duration)
		})

	t.client = telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath()},
		Device:         deviceConfig(),
		UpdateHandler: telegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error {
			return handler.Handle(ctx, u)
		}),
		Middlewares: []telegram.Middleware{
			t.waiter,
			// Smooth bursts of reads; Telegram's limits are per account.
			ratelimit.New(rate.Every(150*time.Millisecond), 8),
			// Feed results of our own calls (sent messages, read marks) into
			// the gap manager so its pts stays consistent.
			updhook.UpdateHook(func(ctx context.Context, u tg.UpdatesClass) error { return handler.Handle(ctx, u) }),
			updhook.AffectedHook(affectedFunc(func(ctx context.Context, ch int64, pts, n int) error {
				return t.gaps.HandleAffected(ctx, ch, pts, n)
			})),
		},
	})
	t.api = t.client.API()
	t.peers = peers.Options{Storage: store, Cache: &peers.InmemoryCache{}}.Build(t.api)
	t.gaps = updates.New(updates.Config{
		Handler:      dispatcher,
		AccessHasher: t.peers,
		OnChannelTooLong: func(channelID int64) {
			log.Warn("channel update gap too long; some messages were not recorded as events", "channel", channelID)
		},
	})
	handler = t.peers.UpdateHook(t.gaps)
	t.sender = message.NewSender(t.api).WithUploader(uploader.NewUploader(t.api))
	return t
}

type affectedFunc func(ctx context.Context, channelID int64, pts, ptsCount int) error

func (f affectedFunc) HandleAffected(ctx context.Context, channelID int64, pts, ptsCount int) error {
	return f(ctx, channelID, pts, ptsCount)
}

// Run keeps the connection up until ctx ends, reconnecting with backoff.
func (t *Telegram) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := t.waiter.Run(ctx, func(ctx context.Context) error {
			return t.client.Run(ctx, t.session)
		})
		t.setDown(err)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errNotAuthorized) {
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		t.log.Error("telegram connection ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (t *Telegram) session(ctx context.Context) error {
	status, err := t.client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status: %w", err)
	}
	if !status.Authorized {
		return errNotAuthorized
	}
	if err := t.peers.Init(ctx); err != nil {
		return fmt.Errorf("peers init: %w", err)
	}
	self, err := t.peers.Self(ctx)
	if err != nil {
		return fmt.Errorf("get self: %w", err)
	}
	t.log.Info("connected", "user", self.VisibleName(), "id", self.ID())
	// The manager refuses a second Run until reset. Its in-memory state
	// survives Reset, so after a reconnect it fetches the difference and
	// messages that arrived while disconnected still become events.
	t.gaps.Reset()
	return t.gaps.Run(ctx, t.api, self.ID(), updates.AuthOptions{
		OnStart: func(context.Context) { t.setUp(self.Raw()) },
	})
}

func (t *Telegram) setUp(self *tg.User) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.self, t.err = self, nil
	select {
	case <-t.ready:
	default:
		close(t.ready)
	}
}

func (t *Telegram) setDown(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		err = errors.New("disconnected")
	}
	t.err = err
	if errors.Is(err, errNotAuthorized) {
		select {
		case <-t.dead:
		default:
			close(t.dead)
		}
	}
	select {
	case <-t.ready:
		t.ready = make(chan struct{})
	default:
	}
}

// Await blocks until the client is connected, for at most 20s.
func (t *Telegram) Await(ctx context.Context) error {
	t.mu.Lock()
	ready, lastErr := t.ready, t.err
	t.mu.Unlock()
	if errors.Is(lastErr, errNotAuthorized) {
		return lastErr
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case <-ready:
		return nil
	case <-t.dead:
		return errNotAuthorized
	case <-ctx.Done():
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.err != nil {
			return fmt.Errorf("telegram not connected: %w", t.err)
		}
		return errors.New("telegram not connected yet; retry shortly")
	}
}

func (t *Telegram) Self() *tg.User {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.self
}

// record turns a new-message update into an event.
func (t *Telegram) record(ctx context.Context, e tg.Entities, m tg.MessageClass) {
	ents := entitiesFromUpdate(e)
	msg, ok := convertMessage(m, ents, true)
	if !ok {
		return
	}
	nm, _ := m.(interface{ GetPeerID() tg.PeerClass })
	if nm == nil || !t.allow.allowsID(msg.ChatID) {
		return
	}
	if msg.ChatName == "" {
		// Updates don't always carry the chat entity; fall back to the cache.
		if p, err := t.peers.ResolvePeer(ctx, nm.GetPeerID()); err == nil {
			msg.ChatName = p.VisibleName()
		}
	}
	t.events.Append(Event{ChatKind: ents.kind(nm.GetPeerID()), Message: msg})
}

// Login runs the interactive phone/code/2FA flow.
func Login(ctx context.Context, cfg Config, ua auth.UserAuthenticator) (*tg.User, error) {
	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath()},
		Device:         deviceConfig(),
	})
	var self *tg.User
	err := client.Run(ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, auth.NewFlow(ua, auth.SendCodeOptions{})); err != nil {
			return err
		}
		var err error
		self, err = client.Self(ctx)
		return err
	})
	if err == nil {
		err = os.Chmod(cfg.SessionPath(), 0o600)
	}
	return self, err
}
