// Package imapx wraps go-imap/v2 with the operations Umleiter needs: LOGIN,
// SELECT, LIST, header and full fetches, APPEND, Message-ID search, MOVE,
// keyword STORE and IDLE.
package imapx

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"

	"github.com/lhns/umleiter/internal/config"
)

// MsgMeta is the cheap per-message metadata used for dedup-key computation
// (and, on destination scans, for keyword backfill via Flags).
type MsgMeta struct {
	UID          imap.UID
	MessageID    string // trimmed Message-ID header value; "" if absent
	From         string // raw From header value (key synthesis only)
	Subject      string // raw Subject header value (key synthesis only)
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

// Client is one IMAP connection to a configured endpoint.
type Client struct {
	ep     config.Endpoint
	c      *imapclient.Client
	notify chan struct{}

	// selected is the currently selected folder ("" = none), to skip
	// redundant SELECTs.
	selected string
	// guardChunk is the Message-ID batch size for guard searches, halved on
	// server BAD responses.
	guardChunk int
	// arbitraryKeywords: the last SELECTed folder advertised PERMANENTFLAGS \*.
	arbitraryKeywords bool
}

// Dial connects (TLS unless disabled) and logs in. Unilateral mailbox
// updates (EXISTS during IDLE) are surfaced on Notify().
func Dial(ep config.Endpoint) (*Client, error) {
	cl := &Client{ep: ep, notify: make(chan struct{}, 1), guardChunk: defaultGuardChunk}
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

// logoutTimeout bounds the best-effort LOGOUT in Close.
var logoutTimeout = 5 * time.Second

// Close logs out (best effort) and closes the connection.
func (cl *Client) Close() {
	if cl.c == nil {
		return
	}
	// A half-open connection never answers LOGOUT; don't let that wedge the
	// supervisor. Closing the conn unblocks the pending Wait.
	done := make(chan struct{})
	go func() {
		_ = cl.c.Logout().Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(logoutTimeout):
	}
	_ = cl.c.Close()
}

// Notify signals whenever the server reports a mailbox size change.
func (cl *Client) Notify() <-chan struct{} { return cl.notify }

// SelectFolder selects the endpoint's folder; see SelectNamedFolder.
func (cl *Client) SelectFolder() (uidValidity uint32, uidNext uint32, numMessages uint32, err error) {
	return cl.SelectNamedFolder(cl.ep.Folder)
}

// SelectNamedFolder selects a folder and returns its UIDVALIDITY, UIDNEXT and
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

// SearchAllUIDs returns every UID in the currently selected folder.
func (cl *Client) SearchAllUIDs() ([]imap.UID, error) {
	data, err := cl.c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search all on %s: %w", cl.ep.Addr(), err)
	}
	return data.AllUIDs(), nil
}

// SupportsArbitraryKeywords reports whether the most recently selected folder
// advertised PERMANENTFLAGS \*.
func (cl *Client) SupportsArbitraryKeywords() bool { return cl.arbitraryKeywords }

// ListFolders lists all folders, with special-use attributes when the server
// supports RFC 6154.
func (cl *Client) ListFolders() ([]FolderInfo, error) {
	var opts *imap.ListOptions
	if cl.c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	data, err := cl.c.List("", "*", opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders on %s: %w", cl.ep.Addr(), err)
	}
	folders := make([]FolderInfo, 0, len(data))
	for _, d := range data {
		folders = append(folders, FolderInfo{Name: d.Mailbox, Attrs: d.Attrs})
	}
	return folders, nil
}

// ResolveFolder resolves a special-use selector (e.g. `\All`, `\Sent`) to the
// server's actual, possibly localized folder name ("[Google Mail]/Alle
// Nachrichten"). Plain folder names pass through unchanged.
func (cl *Client) ResolveFolder(nameOrSelector string) (string, error) {
	if !strings.HasPrefix(nameOrSelector, `\`) {
		return nameOrSelector, nil
	}
	attr := imap.MailboxAttr(nameOrSelector)
	folders, err := cl.ListFolders()
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if slices.Contains(f.Attrs, attr) {
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("no folder with special-use attribute %q on %s (server caps missing SPECIAL-USE, or attribute not present)", attr, cl.ep.Addr())
}

// ResolveSpecialUse resolves the endpoint's configured folder (see
// ResolveFolder) and makes the result the client's working folder.
func (cl *Client) ResolveSpecialUse() (string, error) {
	name, err := cl.ResolveFolder(cl.ep.Folder)
	if err != nil {
		return "", err
	}
	cl.ep.Folder = name
	return name, nil
}

// EnsureFolder creates the endpoint's folder if it does not exist yet.
func (cl *Client) EnsureFolder() error { return cl.EnsureNamedFolder(cl.ep.Folder) }

// EnsureNamedFolder creates the named folder if it does not exist yet.
func (cl *Client) EnsureNamedFolder(name string) error {
	err := cl.c.Create(name, nil).Wait()
	if err == nil {
		return nil
	}
	// Servers phrase "already exists" differently; check by selecting.
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
// start <= UID <= stop in one round trip, ascending by UID.
func (cl *Client) FetchMetaRange(start, stop imap.UID) ([]MsgMeta, error) {
	return cl.fetchMetas(imap.UIDSet{imap.UIDRange{Start: start, Stop: stop}})
}

// fetchMetas fetches MsgMeta for an arbitrary UID set, ascending by UID.
func (cl *Client) fetchMetas(uidSet imap.UIDSet) ([]MsgMeta, error) {
	cmd := cl.c.Fetch(uidSet, &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
		Flags:        true,
		BodySection:  []*imap.FetchItemBodySection{metaSection},
	})
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
		mid, from, subject := parseMetaHeader(buf.FindBodySection(metaSection))
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
	// Servers may return any order; callers' high-water marks need ascending.
	slices.SortFunc(metas, func(a, b MsgMeta) int { return cmp.Compare(a.UID, b.UID) })
	return metas, nil
}

// defaultGuardChunk is the initial Message-ID count per guard SEARCH.
const defaultGuardChunk = 100

// SearchMessageIDsIn reports which of the given Message-IDs exist in the
// named folder. One UID SEARCH per chunk (an OR-tree of HEADER criteria);
// only chunks with hits cost a header fetch to identify the matches. A BAD
// response (server filter limits vary) halves the chunk size for the rest of
// the session, down to single-id searches.
func (cl *Client) SearchMessageIDsIn(folder string, ids []string) (map[string]bool, error) {
	found := map[string]bool{}
	if len(ids) == 0 {
		return found, nil
	}
	if err := cl.ensureSelected(folder); err != nil {
		return nil, err
	}
	for i := 0; i < len(ids); {
		end := min(i+cl.guardChunk, len(ids))
		data, err := cl.c.UIDSearch(orMessageIDCriteria(ids[i:end]), nil).Wait()
		if err != nil {
			if cl.guardChunk > 1 && isBadResponse(err) {
				cl.guardChunk /= 2
				continue // retry the same span with a smaller chunk
			}
			return nil, fmt.Errorf("batch Message-ID search on %s: %w", cl.ep.Addr(), err)
		}
		i = end
		uids := data.AllUIDs()
		if len(uids) == 0 {
			continue
		}
		metas, err := cl.fetchMetas(imap.UIDSetNum(uids...))
		if err != nil {
			return nil, err
		}
		for j := range metas {
			if metas[j].MessageID != "" {
				found[metas[j].MessageID] = true
			}
		}
	}
	return found, nil
}

// isBadResponse reports whether err is a tagged BAD from the server.
func isBadResponse(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBad
}

// orMessageIDCriteria builds `OR ... HEADER Message-Id x ...` as a balanced
// OR tree: servers cap filter nesting depth (Stalwart: "BAD Too many nested
// filters"), and a balanced tree keeps depth at log2(N).
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

var fullSection = &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[]

// FetchFull fetches the complete message for one UID.
func (cl *Client) FetchFull(uid imap.UID) (*FullMessage, error) {
	var full *FullMessage
	err := cl.FetchFullStream([]imap.UID{uid}, func(m *FullMessage) error {
		if m.UID == uid {
			full = m
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fetch full uid %d: %w", uid, err)
	}
	if full == nil {
		return nil, fmt.Errorf("fetch full uid %d: message vanished or empty", uid)
	}
	return full, nil
}

// FetchFullStream fetches complete messages for the given UIDs in one FETCH,
// calling fn per message as it arrives (one body in memory at a time). An
// error from fn aborts the stream.
func (cl *Client) FetchFullStream(uids []imap.UID, fn func(*FullMessage) error) error {
	if len(uids) == 0 {
		return nil
	}
	cmd := cl.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{fullSection},
	})
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
		raw := buf.FindBodySection(fullSection)
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

// Append appends a message to the endpoint's folder.
func (cl *Client) Append(msg *FullMessage, flags []imap.Flag) error {
	return cl.AppendTo(cl.ep.Folder, msg, flags)
}

// PendingAppend is an issued-but-unconfirmed APPEND (see BeginAppend).
type PendingAppend interface {
	// Wait blocks until the server confirms (or rejects) the append.
	Wait() error
}

type pendingAppend struct {
	cmd    *imapclient.AppendCommand
	folder string
	addr   string
}

func (p *pendingAppend) Wait() error {
	if _, err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("append to %q on %s: %w", p.folder, p.addr, err)
	}
	return nil
}

// BeginAppend writes an APPEND (command + literal) without waiting for the
// tagged response, so several appends can be pipelined.
func (cl *Client) BeginAppend(folder string, msg *FullMessage, flags []imap.Flag) (PendingAppend, error) {
	opts := &imap.AppendOptions{Flags: flags}
	if !msg.InternalDate.IsZero() {
		opts.Time = msg.InternalDate
	}
	cmd := cl.c.Append(folder, int64(len(msg.Raw)), opts)
	if _, err := cmd.Write(msg.Raw); err != nil {
		cmd.Close()
		return nil, fmt.Errorf("append write to %s: %w", cl.ep.Addr(), err)
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("append close to %s: %w", cl.ep.Addr(), err)
	}
	return &pendingAppend{cmd: cmd, folder: folder, addr: cl.ep.Addr()}, nil
}

// AppendTo appends a message to the named folder, preserving INTERNALDATE,
// and waits for the server's confirmation.
func (cl *Client) AppendTo(folder string, msg *FullMessage, flags []imap.Flag) error {
	p, err := cl.BeginAppend(folder, msg, flags)
	if err != nil {
		return err
	}
	return p.Wait()
}

// searchMessageID selects folder and returns the UIDs whose Message-ID
// header matches.
func (cl *Client) searchMessageID(folder, messageID string) ([]imap.UID, error) {
	if err := cl.ensureSelected(folder); err != nil {
		return nil, err
	}
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: messageID}},
	}
	data, err := cl.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search Message-ID on %s: %w", cl.ep.Addr(), err)
	}
	return data.AllUIDs(), nil
}

// MoveMessageID moves the message with the given Message-ID between folders.
// Returns (false, nil) when it is not in fromFolder (e.g. refiled by the
// user). Falls back to COPY + STORE \Deleted + EXPUNGE without MOVE support.
func (cl *Client) MoveMessageID(fromFolder, toFolder, messageID string) (bool, error) {
	uids, err := cl.searchMessageID(fromFolder, messageID)
	if err != nil || len(uids) == 0 {
		return false, err
	}
	if _, err := cl.c.Move(imap.UIDSetNum(uids...), toFolder).Wait(); err != nil {
		return false, fmt.Errorf("move %q -> %q on %s: %w", fromFolder, toFolder, cl.ep.Addr(), err)
	}
	return true, nil
}

// MoveUIDs moves messages by UID between folders; chunking is the caller's
// concern.
func (cl *Client) MoveUIDs(fromFolder string, uids []imap.UID, toFolder string) error {
	if len(uids) == 0 {
		return nil
	}
	if err := cl.ensureSelected(fromFolder); err != nil {
		return err
	}
	if _, err := cl.c.Move(imap.UIDSetNum(uids...), toFolder).Wait(); err != nil {
		return fmt.Errorf("move %d uids %q -> %q on %s: %w", len(uids), fromFolder, toFolder, cl.ep.Addr(), err)
	}
	cl.selected = "" // message set changed; be conservative
	return nil
}

// StoreKeywordByMessageID adds or removes a keyword on the message with the
// given Message-ID in folder. Returns (false, nil) when it is not there.
func (cl *Client) StoreKeywordByMessageID(folder, messageID string, add bool, kw imap.Flag) (bool, error) {
	uids, err := cl.searchMessageID(folder, messageID)
	if err != nil || len(uids) == 0 {
		return false, err
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	if err := cl.storeFlags(uids, op, []imap.Flag{kw}); err != nil {
		return false, err
	}
	return true, nil
}

// StoreKeywordsUIDs adds keywords to messages by UID in the currently
// selected folder.
func (cl *Client) StoreKeywordsUIDs(uids []imap.UID, kws []imap.Flag) error {
	if len(uids) == 0 || len(kws) == 0 {
		return nil
	}
	return cl.storeFlags(uids, imap.StoreFlagsAdd, kws)
}

func (cl *Client) storeFlags(uids []imap.UID, op imap.StoreFlagsOp, flags []imap.Flag) error {
	cmd := cl.c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil)
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("store keywords on %s: %w", cl.ep.Addr(), err)
	}
	return nil
}

// Idle starts IDLE on the currently selected folder; stop it with Close().
// go-imap restarts the underlying command every ~28 minutes.
func (cl *Client) Idle() (*imapclient.IdleCommand, error) {
	return cl.c.Idle()
}

// parseMetaHeader extracts the trimmed Message-ID, From and Subject values
// from a HEADER.FIELDS response.
func parseMetaHeader(hdr []byte) (messageID, from, subject string) {
	if len(hdr) == 0 {
		return "", "", ""
	}
	// message.Read may return a non-fatal error (e.g. unknown charset) along
	// with a usable entity; only bail without one.
	ent, _ := message.Read(bytes.NewReader(append(hdr, '\r', '\n')))
	if ent == nil {
		return "", "", ""
	}
	h := ent.Header
	return strings.TrimSpace(h.Get("Message-Id")), strings.TrimSpace(h.Get("From")), strings.TrimSpace(h.Get("Subject"))
}
