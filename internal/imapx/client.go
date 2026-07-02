// Package imapx wraps go-imap/v2 with the handful of operations Umleiter
// needs: IMAPS connect + LOGIN, SELECT, windowed header fetch, full fetch,
// APPEND with INTERNALDATE, HEADER search, folder create, and IDLE.
package imapx

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"

	"github.com/lhns/umleitung/internal/config"
)

// MsgMeta is the cheap per-message metadata used for dedup-key computation.
type MsgMeta struct {
	UID          imap.UID
	MessageID    string // trimmed raw Message-ID header value; "" if absent
	From         string // raw From header value (used only for key synthesis)
	Subject      string // raw Subject header value (used only for key synthesis)
	InternalDate time.Time
	Size         int64
}

// FullMessage is a complete message ready to be appended to the destination.
type FullMessage struct {
	Raw          []byte
	Flags        []imap.Flag
	InternalDate time.Time
}

// Client is one IMAPS connection to a configured endpoint.
type Client struct {
	ep     config.Endpoint
	c      *imapclient.Client
	notify chan struct{}
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
	c, err := imapclient.DialTLS(ep.Addr(), opts)
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
	data, err := cl.c.Select(cl.ep.Folder, nil).Wait()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("select %q on %s: %w", cl.ep.Folder, cl.ep.Addr(), err)
	}
	return data.UIDValidity, uint32(data.UIDNext), data.NumMessages, nil
}

// EnsureFolder creates the endpoint's folder if it does not exist yet.
func (cl *Client) EnsureFolder() error {
	err := cl.c.Create(cl.ep.Folder, nil).Wait()
	if err == nil {
		return nil
	}
	// Treat "already exists" as success; servers phrase this differently, so
	// double-check by selecting.
	if _, _, _, selErr := cl.SelectFolder(); selErr == nil {
		return nil
	}
	return fmt.Errorf("create %q on %s: %w", cl.ep.Folder, cl.ep.Addr(), err)
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
	uidSet := imap.UIDSet{imap.UIDRange{Start: start, Stop: stop}}
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
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
			return nil, fmt.Errorf("fetch meta %d:%d: %w", start, stop, err)
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
		})
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("fetch meta %d:%d: %w", start, stop, err)
	}
	// Servers may return any order; sort ascending so the caller's
	// high-water-mark logic is correct.
	sortMetas(metas)
	return metas, nil
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

// HasMessageID searches the selected folder for a Message-ID header value.
func (cl *Client) HasMessageID(messageID string) (bool, error) {
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: messageID}},
	}
	data, err := cl.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return false, fmt.Errorf("search Message-ID on %s: %w", cl.ep.Addr(), err)
	}
	return len(data.AllUIDs()) > 0, nil
}

// Append appends a raw message to the endpoint's folder, preserving
// INTERNALDATE and the given flags. Returns only after the server confirms.
func (cl *Client) Append(msg *FullMessage, flags []imap.Flag) error {
	opts := &imap.AppendOptions{Flags: flags}
	if !msg.InternalDate.IsZero() {
		opts.Time = msg.InternalDate
	}
	cmd := cl.c.Append(cl.ep.Folder, int64(len(msg.Raw)), opts)
	if _, err := cmd.Write(msg.Raw); err != nil {
		cmd.Close()
		return fmt.Errorf("append write to %s: %w", cl.ep.Addr(), err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("append close to %s: %w", cl.ep.Addr(), err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("append to %q on %s: %w", cl.ep.Folder, cl.ep.Addr(), err)
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
