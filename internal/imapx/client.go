// Package imapx wraps go-imap/v2 with the handful of operations Umleiter
// needs: IMAPS connect + LOGIN, SELECT, windowed header fetch, full fetch,
// APPEND with INTERNALDATE, HEADER search, folder create, and IDLE.
package imapx

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"

	"github.com/lhns/umleitung/internal/config"
)

// MsgMeta is the cheap per-message metadata used for dedup-key computation
// (and, on destination scans, for keyword backfill via Flags).
type MsgMeta struct {
	UID          imap.UID
	MessageID    string // trimmed raw Message-ID header value; "" if absent
	From         string // raw From header value (used only for key synthesis)
	Subject      string // raw Subject header value (used only for key synthesis)
	InternalDate time.Time
	Size         int64
	Flags        []imap.Flag
}

// FullMessage is a complete message ready to be appended to the destination.
type FullMessage struct {
	UID          imap.UID
	Raw          []byte
	Flags        []imap.Flag
	InternalDate time.Time
}

// FolderInfo describes one folder returned by LIST.
type FolderInfo struct {
	Name  string
	Attrs []imap.MailboxAttr
}

// Client is one IMAPS connection to a configured endpoint.
type Client struct {
	ep     config.Endpoint
	c      *imapclient.Client
	notify chan struct{}

	// selected is the currently selected folder ("" = none yet); used to
	// skip redundant SELECT round trips.
	selected string
	// arbitraryKeywords records whether the last SELECTed mailbox advertised
	// PERMANENTFLAGS \* (arbitrary keywords storable).
	arbitraryKeywords bool
}

// Dial connects with TLS, waits for the greeting and logs in with LOGIN.
// Unilateral mailbox updates (EXISTS during IDLE) are surfaced on Notify().
func Dial(ep config.Endpoint) (*Client, error) {
	cl := &Client{ep: ep, notify: make(chan struct{}, 1)}
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					select {
					case cl.notify <- struct{}{}:
					default: // already pending
					}
				}
			},
		},
	}
	dial := imapclient.DialTLS
	if !ep.TLS {
		dial = imapclient.DialInsecure // local testing only
	}
	c, err := dial(ep.Addr(), opts)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", ep.Addr(), err)
	}
	if err := c.Login(ep.User, ep.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("login %s as %s: %w", ep.Addr(), ep.User, err)
	}
	cl.c = c
	return cl, nil
}

// Close logs out (best effort) and closes the connection.
func (cl *Client) Close() {
	if cl.c != nil {
		_ = cl.c.Logout().Wait()
		_ = cl.c.Close()
	}
}

// Notify delivers a signal whenever the server reports a mailbox change
// (e.g. new mail while IDLE is running).
func (cl *Client) Notify() <-chan struct{} { return cl.notify }

// SelectFolder selects the endpoint's folder and returns its UIDVALIDITY,
// UIDNEXT and message count.
func (cl *Client) SelectFolder() (uidValidity uint32, uidNext uint32, numMessages uint32, err error) {
	return cl.SelectNamedFolder(cl.ep.Folder)
}

// SelectNamedFolder selects an arbitrary folder (used by the membership scan
// and multi-folder destinations) and returns its UIDVALIDITY, UIDNEXT and
// message count.
func (cl *Client) SelectNamedFolder(name string) (uidValidity uint32, uidNext uint32, numMessages uint32, err error) {
	data, err := cl.c.Select(name, nil).Wait()
	if err != nil {
		cl.selected = ""
		return 0, 0, 0, fmt.Errorf("select %q on %s: %w", name, cl.ep.Addr(), err)
	}
	cl.selected = name
	cl.arbitraryKeywords = slices.Contains(data.PermanentFlags, imap.FlagWildcard)
	return data.UIDValidity, uint32(data.UIDNext), data.NumMessages, nil
}

// ensureSelected selects the folder only if it is not already selected.
func (cl *Client) ensureSelected(name string) error {
	if cl.selected == name {
		return nil
	}
	_, _, _, err := cl.SelectNamedFolder(name)
	return err
}

// SearchAllUIDs returns every UID in the currently selected folder — a cheap
// full-membership snapshot (UIDs only, no headers).
func (cl *Client) SearchAllUIDs() ([]imap.UID, error) {
	data, err := cl.c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search all on %s: %w", cl.ep.Addr(), err)
	}
	return data.AllUIDs(), nil
}

// SupportsArbitraryKeywords reports whether the most recently selected folder
// advertised PERMANENTFLAGS \* (arbitrary keywords may be stored).
func (cl *Client) SupportsArbitraryKeywords() bool { return cl.arbitraryKeywords }

// ListFolders lists all folders on the server, with special-use attributes
// when the server supports RFC 6154.
func (cl *Client) ListFolders() ([]FolderInfo, error) {
	var opts *imap.ListOptions
	if cl.c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	cmd := cl.c.List("", "*", opts)
	data, err := cmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders on %s: %w", cl.ep.Addr(), err)
	}
	folders := make([]FolderInfo, 0, len(data))
	for _, d := range data {
		folders = append(folders, FolderInfo{Name: d.Mailbox, Attrs: d.Attrs})
	}
	return folders, nil
}

// ResolveSpecialUse resolves a special-use folder selector in the endpoint's
// configured folder (e.g. `\All`, `\Sent` — RFC 6154) to the server's actual
// folder name, which providers localize (German Gmail exposes All Mail as
// "[Gmail]/Alle Nachrichten"). Plain folder names pass through unchanged.
// The resolved name becomes the client's working folder.
func (cl *Client) ResolveSpecialUse() (string, error) {
	if !strings.HasPrefix(cl.ep.Folder, `\`) {
		return cl.ep.Folder, nil
	}
	attr := imap.MailboxAttr(cl.ep.Folder)
	folders, err := cl.ListFolders()
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if slices.Contains(f.Attrs, attr) {
			cl.ep.Folder = f.Name
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("no folder with special-use attribute %q on %s (server caps missing SPECIAL-USE, or attribute not present)", attr, cl.ep.Addr())
}

// EnsureFolder creates the endpoint's default folder if it does not exist yet.
func (cl *Client) EnsureFolder() error { return cl.EnsureNamedFolder(cl.ep.Folder) }

// EnsureNamedFolder creates the named folder if it does not exist yet.
func (cl *Client) EnsureNamedFolder(name string) error {
	err := cl.c.Create(name, nil).Wait()
	if err == nil {
		return nil
	}
	// Treat "already exists" as success; servers phrase this differently, so
	// double-check by selecting.
	if _, _, _, selErr := cl.SelectNamedFolder(name); selErr == nil {
		return nil
	}
	return fmt.Errorf("create %q on %s: %w", name, cl.ep.Addr(), err)
}

var metaSection = &imap.FetchItemBodySection{
	Specifier:    imap.PartSpecifierHeader,
	HeaderFields: []string{"Message-Id", "From", "Subject"},
	Peek:         true,
}

// FetchMetaRange fetches MsgMeta for every existing message with
// start <= UID <= stop, in one FETCH round trip, ascending by UID.
// Non-existent UIDs in the range are simply absent from the result.
func (cl *Client) FetchMetaRange(start, stop imap.UID) ([]MsgMeta, error) {
	return cl.fetchMetas(imap.UIDSet{imap.UIDRange{Start: start, Stop: stop}})
}

// fetchMetas fetches MsgMeta for an arbitrary UID set, ascending by UID.
func (cl *Client) fetchMetas(uidSet imap.UIDSet) ([]MsgMeta, error) {
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
		Flags:        true,
		BodySection:  []*imap.FetchItemBodySection{metaSection},
	}
	cmd := cl.c.Fetch(uidSet, fetchOpts)
	var metas []MsgMeta
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			cmd.Close()
			return nil, fmt.Errorf("fetch metas: %w", err)
		}
		hdr := buf.FindBodySection(metaSection)
		mid, from, subject := parseMetaHeader(hdr)
		metas = append(metas, MsgMeta{
			UID:          buf.UID,
			MessageID:    mid,
			From:         from,
			Subject:      subject,
			InternalDate: buf.InternalDate,
			Size:         buf.RFC822Size,
			Flags:        buf.Flags,
		})
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("fetch metas: %w", err)
	}
	// Servers may return any order; sort ascending so the caller's
	// high-water-mark logic is correct.
	sortMetas(metas)
	return metas, nil
}

// guardChunk bounds Message-IDs per SEARCH command (command-length limits).
const guardChunk = 100

// SearchMessageIDsIn reports which of the given Message-IDs exist in the
// named folder. Batched: one `UID SEARCH` per chunk of ~100 ids (an OR-tree
// of HEADER criteria); only chunks with hits cost an extra header fetch to
// identify which ids matched. Used by the mirror's destination guard.
func (cl *Client) SearchMessageIDsIn(folder string, ids []string) (map[string]bool, error) {
	found := map[string]bool{}
	if len(ids) == 0 {
		return found, nil
	}
	if err := cl.ensureSelected(folder); err != nil {
		return nil, err
	}
	for chunk := range slices.Chunk(ids, guardChunk) {
		data, err := cl.c.UIDSearch(orMessageIDCriteria(chunk), nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("batch Message-ID search on %s: %w", cl.ep.Addr(), err)
		}
		uids := data.AllUIDs()
		if len(uids) == 0 {
			continue
		}
		metas, err := cl.fetchMetas(imap.UIDSetNum(uids...))
		if err != nil {
			return nil, err
		}
		for i := range metas {
			if metas[i].MessageID != "" {
				found[metas[i].MessageID] = true
			}
		}
	}
	return found, nil
}

// orMessageIDCriteria builds `OR ... OR HEADER Message-Id x ...` as a
// BALANCED OR-pair tree: servers cap filter nesting depth (Stalwart rejects
// deep chains with "BAD Too many nested filters"), and a balanced tree keeps
// depth at log2(N) — 7 levels for a 100-id chunk instead of 99.
func orMessageIDCriteria(ids []string) *imap.SearchCriteria {
	if len(ids) == 1 {
		return &imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: ids[0]}},
		}
	}
	mid := len(ids) / 2
	return &imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{*orMessageIDCriteria(ids[:mid]), *orMessageIDCriteria(ids[mid:])}},
	}
}

// FetchFull fetches the complete raw message, flags and INTERNALDATE for one UID.
func (cl *Client) FetchFull(uid imap.UID) (*FullMessage, error) {
	bodySection := &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[] = whole message
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{bodySection},
	}
	cmd := cl.c.Fetch(imap.UIDSetNum(uid), fetchOpts)
	defer cmd.Close()
	var full *FullMessage
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, fmt.Errorf("fetch full uid %d: %w", uid, err)
		}
		if buf.UID != uid {
			continue
		}
		full = &FullMessage{
			Raw:          buf.FindBodySection(bodySection),
			Flags:        buf.Flags,
			InternalDate: buf.InternalDate,
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("fetch full uid %d: %w", uid, err)
	}
	if full == nil || len(full.Raw) == 0 {
		return nil, fmt.Errorf("fetch full uid %d: message vanished or empty", uid)
	}
	return full, nil
}

// FetchFullStream fetches complete messages for the given UIDs in ONE FETCH
// command, invoking fn per message as bodies stream in (bounded memory: one
// message buffered at a time). fn returning an error aborts the stream.
func (cl *Client) FetchFullStream(uids []imap.UID, fn func(*FullMessage) error) error {
	if len(uids) == 0 {
		return nil
	}
	bodySection := &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[] = whole message
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{bodySection},
	}
	cmd := cl.c.Fetch(imap.UIDSetNum(uids...), fetchOpts)
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			cmd.Close()
			return fmt.Errorf("fetch full stream: %w", err)
		}
		raw := buf.FindBodySection(bodySection)
		if len(raw) == 0 {
			continue // vanished mid-fetch; next reconcile retries
		}
		if err := fn(&FullMessage{
			UID:          buf.UID,
			Raw:          raw,
			Flags:        buf.Flags,
			InternalDate: buf.InternalDate,
		}); err != nil {
			cmd.Close()
			return err
		}
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("fetch full stream: %w", err)
	}
	return nil
}

// HasMessageID searches the currently selected folder for a Message-ID
// header value.
func (cl *Client) HasMessageID(messageID string) (bool, error) {
	uids, err := cl.searchMessageIDUIDs(messageID)
	return len(uids) > 0, err
}

// Append appends a raw message to the endpoint's default folder.
func (cl *Client) Append(msg *FullMessage, flags []imap.Flag) error {
	return cl.AppendTo(cl.ep.Folder, msg, flags)
}

// AppendTo appends a raw message to the named folder, preserving
// INTERNALDATE and the given flags. Returns only after the server confirms.
func (cl *Client) AppendTo(folder string, msg *FullMessage, flags []imap.Flag) error {
	opts := &imap.AppendOptions{Flags: flags}
	if !msg.InternalDate.IsZero() {
		opts.Time = msg.InternalDate
	}
	cmd := cl.c.Append(folder, int64(len(msg.Raw)), opts)
	if _, err := cmd.Write(msg.Raw); err != nil {
		cmd.Close()
		return fmt.Errorf("append write to %s: %w", cl.ep.Addr(), err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("append close to %s: %w", cl.ep.Addr(), err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("append to %q on %s: %w", folder, cl.ep.Addr(), err)
	}
	return nil
}

// searchMessageIDUIDs returns the UIDs matching a Message-ID header in the
// currently selected folder.
func (cl *Client) searchMessageIDUIDs(messageID string) ([]imap.UID, error) {
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: messageID}},
	}
	data, err := cl.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search Message-ID on %s: %w", cl.ep.Addr(), err)
	}
	return data.AllUIDs(), nil
}

// HasMessageIDIn searches the named folder for a Message-ID header value.
func (cl *Client) HasMessageIDIn(folder, messageID string) (bool, error) {
	if err := cl.ensureSelected(folder); err != nil {
		return false, err
	}
	uids, err := cl.searchMessageIDUIDs(messageID)
	return len(uids) > 0, err
}

// MoveMessageID moves the message with the given Message-ID from one folder
// to another. Returns (false, nil) when the message is not in fromFolder —
// e.g. manually refiled by the user — which callers treat as "nothing to do".
// go-imap falls back to COPY + STORE \Deleted + EXPUNGE on servers without
// the MOVE capability.
func (cl *Client) MoveMessageID(fromFolder, toFolder, messageID string) (bool, error) {
	if err := cl.ensureSelected(fromFolder); err != nil {
		return false, err
	}
	uids, err := cl.searchMessageIDUIDs(messageID)
	if err != nil {
		return false, err
	}
	if len(uids) == 0 {
		return false, nil
	}
	if _, err := cl.c.Move(imap.UIDSetNum(uids...), toFolder).Wait(); err != nil {
		return false, fmt.Errorf("move %q -> %q on %s: %w", fromFolder, toFolder, cl.ep.Addr(), err)
	}
	return true, nil
}

// MoveUIDs batch-moves messages by UID from the currently selected folder.
// Used by the placement backfill; chunking is the caller's concern.
func (cl *Client) MoveUIDs(uids []imap.UID, toFolder string) error {
	if len(uids) == 0 {
		return nil
	}
	if _, err := cl.c.Move(imap.UIDSetNum(uids...), toFolder).Wait(); err != nil {
		return fmt.Errorf("move %d uids to %q on %s: %w", len(uids), toFolder, cl.ep.Addr(), err)
	}
	cl.selected = "" // message set changed; be conservative
	return nil
}

// StoreKeywordByMessageID adds or removes a keyword flag on the message with
// the given Message-ID in the named folder. Returns (false, nil) when the
// message is not found there. Idempotent (±FLAGS.SILENT).
func (cl *Client) StoreKeywordByMessageID(folder, messageID string, add bool, kw imap.Flag) (bool, error) {
	if err := cl.ensureSelected(folder); err != nil {
		return false, err
	}
	uids, err := cl.searchMessageIDUIDs(messageID)
	if err != nil {
		return false, err
	}
	if len(uids) == 0 {
		return false, nil
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	cmd := cl.c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{
		Op: op, Silent: true, Flags: []imap.Flag{kw},
	}, nil)
	if err := cmd.Close(); err != nil {
		return false, fmt.Errorf("store keyword on %s: %w", cl.ep.Addr(), err)
	}
	return true, nil
}

// StoreKeywordsUIDs batch-adds keyword flags to messages by UID in the
// currently selected folder (keyword backfill).
func (cl *Client) StoreKeywordsUIDs(uids []imap.UID, kws []imap.Flag) error {
	if len(uids) == 0 || len(kws) == 0 {
		return nil
	}
	cmd := cl.c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: kws,
	}, nil)
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("store keywords on %s: %w", cl.ep.Addr(), err)
	}
	return nil
}

// Idle starts IDLE on the currently selected folder. go-imap restarts the
// underlying IDLE command every ~28 minutes on its own; the caller stops it
// via Close() when it wants to run a reconcile.
func (cl *Client) Idle() (*imapclient.IdleCommand, error) {
	return cl.c.Idle()
}

// parseMetaHeader extracts the raw Message-ID, From and Subject values from a
// HEADER.FIELDS response using go-message.
func parseMetaHeader(hdr []byte) (messageID, from, subject string) {
	if len(hdr) == 0 {
		return "", "", ""
	}
	// message.Read may return a non-fatal error (e.g. unknown charset) while
	// still yielding a usable entity; only bail if we got no entity at all.
	ent, _ := message.Read(bytes.NewReader(append(hdr, '\r', '\n')))
	if ent == nil {
		return "", "", ""
	}
	h := ent.Header
	messageID = strings.TrimSpace(h.Get("Message-Id"))
	from = strings.TrimSpace(h.Get("From"))
	subject = strings.TrimSpace(h.Get("Subject"))
	return messageID, from, subject
}

func sortMetas(metas []MsgMeta) {
	sort.Slice(metas, func(i, j int) bool { return metas[i].UID < metas[j].UID })
}
