package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

func TestMarkedID(t *testing.T) {
	cases := []struct {
		p    tg.PeerClass
		want int64
	}{
		{&tg.PeerUser{UserID: 42}, 42},
		{&tg.PeerChat{ChatID: 42}, -42},
		{&tg.PeerChannel{ChannelID: 42}, -1000000000042},
	}
	for _, c := range cases {
		if got := markedID(c.p); got != c.want {
			t.Errorf("markedID(%T) = %d, want %d", c.p, got, c.want)
		}
	}
}

func TestPeerStorageRoundTrip(t *testing.T) {
	s, err := openPeerStorage(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	k := peers.Key{Prefix: "users_", ID: -7}
	if _, found, _ := s.Find(ctx, k); found {
		t.Fatal("found before save")
	}
	if err := s.Save(ctx, k, peers.Value{AccessHash: -123456789}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePhone(ctx, "+1555", k); err != nil {
		t.Fatal(err)
	}
	v, found, err := s.Find(ctx, k)
	if err != nil || !found || v.AccessHash != -123456789 {
		t.Fatalf("Find = %v %v %v", v, found, err)
	}
	gk, gv, found, err := s.FindPhone(ctx, "+1555")
	if err != nil || !found || gk != k || gv.AccessHash != -123456789 {
		t.Fatalf("FindPhone = %v %v %v %v", gk, gv, found, err)
	}
	if err := s.SaveContactsHash(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.GetContactsHash(ctx); h != 99 {
		t.Fatalf("contacts hash = %d", h)
	}
}

func ev(chat int64, out bool) Event {
	return Event{Message: Message{ChatID: chat, Out: out}}
}

func TestEventLogWait(t *testing.T) {
	l := NewEventLog(3)
	all := func(Event) bool { return true }
	ctx := context.Background()

	// From now: nothing yet, times out.
	r := l.Wait(ctx, "", -1, 10*time.Millisecond, 10, all)
	if len(r.Events) != 0 || r.LastSeq != 0 || r.Boot == "" {
		t.Fatalf("empty wait = %+v", r)
	}

	// A waiter blocks until an append.
	done := make(chan WaitResult)
	go func() { done <- l.Wait(ctx, r.Boot, r.LastSeq, time.Second, 10, all) }()
	time.Sleep(20 * time.Millisecond)
	l.Append(ev(1, false))
	r = <-done
	if len(r.Events) != 1 || r.Events[0].Seq != 1 || r.LastSeq != 1 {
		t.Fatalf("woken wait = %+v", r)
	}

	// Filtered-out events still advance last_seq.
	l.Append(ev(2, true))
	r = l.Wait(ctx, r.Boot, r.LastSeq, 10*time.Millisecond, 10, func(e Event) bool { return !e.Out })
	if len(r.Events) != 0 || r.LastSeq != 2 {
		t.Fatalf("filtered wait = %+v", r)
	}

	// Overflow: capacity 3, append 4 more → seqs 3..6 written, 4..6 kept.
	for i := 0; i < 4; i++ {
		l.Append(ev(1, false))
	}
	r = l.Wait(ctx, r.Boot, 2, 10*time.Millisecond, 10, all)
	if !r.Gap || len(r.Events) != 3 || r.Events[0].Seq != 4 || r.LastSeq != 6 {
		t.Fatalf("gap wait = %+v", r)
	}

	// Limit, then resume where it stopped.
	r = l.Wait(ctx, r.Boot, 3, 10*time.Millisecond, 2, all)
	if len(r.Events) != 2 || r.LastSeq != 5 {
		t.Fatalf("limited wait = %+v", r)
	}

	// Different boot → reset.
	r = l.Wait(ctx, "stale", 0, 10*time.Millisecond, 10, all)
	if !r.Reset || len(r.Events) != 0 || r.LastSeq != 6 {
		t.Fatalf("reset wait = %+v", r)
	}
}

func TestFileGuard(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "ok.txt"))
	write(filepath.Join(outside, "secret.txt"))
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	g, err := newFileGuard(Config{AllowedRoots: []string{root}, DownloadDir: filepath.Join(base, "dl")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.readable(filepath.Join(root, "ok.txt")); err != nil {
		t.Errorf("inside root rejected: %v", err)
	}
	for _, bad := range []string{
		filepath.Join(outside, "secret.txt"),
		filepath.Join(root, "link.txt"), // symlink escaping the root
		filepath.Join(root, "..", "outside", "secret.txt"),
		root, // directory
	} {
		if _, _, err := g.readable(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}

	p1, err := g.downloadPath("../../etc/passwd")
	if err != nil || filepath.Dir(p1) != g.download || filepath.Base(p1) != "passwd" {
		t.Fatalf("downloadPath = %q, %v", p1, err)
	}
	p2, _ := g.downloadPath("passwd")
	if p2 == p1 {
		t.Fatalf("downloadPath reused %q", p1)
	}
}

func TestChatFilter(t *testing.T) {
	f, err := newChatFilter(nil)
	if err != nil || f != nil || !f.allowsID(5) {
		t.Fatal("empty filter should allow everything")
	}
	f, err = newChatFilter([]string{"123", "-100999", "@Some_User"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.allowsID(123) || !f.allowsID(-100999) || f.allowsID(999) {
		t.Fatal("id matching wrong")
	}
	if !f.usernames["some_user"] {
		t.Fatal("username not normalized")
	}
	if _, err := newChatFilter([]string{"two words"}); err == nil {
		t.Fatal("bad entry accepted")
	}
}

func TestConvertMessage(t *testing.T) {
	e := newEntities(
		[]tg.UserClass{&tg.User{ID: 7, FirstName: "Sam", LastName: "Lee"}},
		[]tg.ChatClass{&tg.Channel{ID: 9, Title: "News", Broadcast: true}},
	)
	// Incoming private message: sender is the chat's user.
	m, ok := convertMessage(&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: "hi", Date: 1700000000}, e, true)
	if !ok || m.FromName != "Sam Lee" || m.FromID != 7 || m.ChatID != 7 || m.Date != "2023-11-14T22:13:20Z" {
		t.Fatalf("private = %+v", m)
	}
	// Outgoing: no sender filled.
	m, _ = convertMessage(&tg.Message{ID: 2, Out: true, PeerID: &tg.PeerUser{UserID: 7}}, e, false)
	if m.FromID != 0 || !m.Out {
		t.Fatalf("outgoing = %+v", m)
	}
	// Channel post with a voice note.
	m, _ = convertMessage(&tg.Message{ID: 3, PeerID: &tg.PeerChannel{ChannelID: 9}, Media: &tg.MessageMediaDocument{
		Document: &tg.Document{MimeType: "audio/ogg", Size: 10, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true, Duration: 4}}},
	}}, e, true)
	if m.FromName != "News" || m.Media == nil || m.Media.Kind != "voice" || m.Media.Duration != 4 || e.kind(&tg.PeerChannel{ChannelID: 9}) != "channel" {
		t.Fatalf("channel voice = %+v %+v", m, m.Media)
	}
}
