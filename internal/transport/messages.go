package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

const messageTimeLayout = "2006-01-02T15:04:05.000000000Z"

var errMessageCursor = errors.New("invalid message cursor")

type messageView struct {
	ID        string  `json:"id"`
	SessionID string  `json:"session_id"`
	RunID     *string `json:"run_id"`
	TurnID    *string `json:"turn_id"`
	Role      string  `json:"role"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	Content   string  `json:"content"`
	Event     string  `json:"event"`
	CreatedAt string  `json:"created_at"`
}

type messagePageView struct {
	Messages   []messageView `json:"messages"`
	NextCursor string        `json:"next_cursor"`
}

// Field order is part of the cursor v1 encoding contract.
type messageCursorView struct {
	Version   int    `json:"v"`
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func (s *Server) listSessionMessages(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message query"))
		return
	}
	limit := 50
	if values, ok := query["limit"]; ok {
		if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 3 || values[0][0] < '1' || values[0][0] > '9' {
			writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message limit"))
			return
		}
		for _, c := range values[0] {
			if c < '0' || c > '9' {
				writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message limit"))
				return
			}
		}
		limit, _ = strconv.Atoi(values[0]) // At most three ASCII digits.
		if limit > 200 {
			writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message limit"))
			return
		}
	}
	sessionID := r.PathValue("id")
	var after *storage.MessageCursor
	if values, ok := query["cursor"]; ok {
		if len(values) != 1 {
			writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message cursor"))
			return
		}
		if values[0] != "" {
			after, err = decodeMessageCursor(values[0], sessionID)
			if err != nil {
				writeErr(w, domain.E(domain.CodeInvalidArgument, "invalid message cursor"))
				return
			}
		}
	}
	ctx := r.Context()
	if _, err := s.Manager.Store.GetSession(ctx, sessionID); err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			writeErr(w, domain.E(domain.CodeNotFound, "record not found").WithRef(sessionID))
		} else {
			writeErr(w, domain.E("internal", "request failed"))
		}
		return
	}
	// The store owns the consistent read, ordering, and look-ahead. No anchor
	// lookup or transcript reconstruction is needed, even for a deleted ID.
	page, err := s.Manager.Store.ListMessages(ctx, sessionID, after, limit)
	if err != nil {
		writeErr(w, domain.E("internal", "request failed"))
		return
	}
	out := messagePageView{Messages: make([]messageView, 0, len(page.Messages))}
	for _, message := range page.Messages {
		created, err := formatMessageTime(message.CreatedAt)
		if err != nil {
			writeErr(w, domain.E("internal", "request failed"))
			return
		}
		out.Messages = append(out.Messages, messageView{
			ID: message.ID, SessionID: message.SessionID,
			RunID: messageAssociation(message.RunID), TurnID: messageAssociation(message.TurnID),
			Role: message.Role, Kind: message.Kind, Status: message.Status,
			Content: message.Content, Event: message.Event, CreatedAt: created,
		})
	}
	if page.NextCursor != nil {
		out.NextCursor, err = encodeMessageCursor(sessionID, *page.NextCursor)
		if err != nil {
			writeErr(w, domain.E("internal", "request failed"))
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func messageAssociation(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

func parseMessageTime(value string) (time.Time, error) {
	created, err := time.Parse(messageTimeLayout, value)
	if err != nil || created.UTC().Format(messageTimeLayout) != value {
		return time.Time{}, errMessageCursor
	}
	return created, nil
}

func formatMessageTime(created time.Time) (string, error) {
	value := created.UTC().Format(messageTimeLayout)
	if _, err := parseMessageTime(value); err != nil {
		return "", err
	}
	return value, nil
}

func asciiMessageID(id string) bool {
	if id == "" {
		return false
	}
	for i := range id {
		if id[i] > 127 {
			return false
		}
	}
	return true
}

func decodeMessageCursor(value, sessionID string) (*storage.MessageCursor, error) {
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return nil, errMessageCursor
		}
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || !utf8.Valid(data) {
		return nil, errMessageCursor
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, errMessageCursor
	}
	seen := make(map[string]bool, 4)
	var created, id string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, errMessageCursor
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return nil, errMessageCursor
		}
		seen[name] = true
		value, err := dec.Token()
		if err != nil {
			return nil, errMessageCursor
		}
		switch name {
		case "v":
			if number, ok := value.(json.Number); !ok || number.String() != "1" {
				return nil, errMessageCursor
			}
		case "session_id":
			if text, ok := value.(string); !ok || text == "" || text != sessionID {
				return nil, errMessageCursor
			}
		case "created_at":
			created, ok = value.(string)
			if !ok {
				return nil, errMessageCursor
			}
		case "id":
			id, ok = value.(string)
			if !ok || !asciiMessageID(id) {
				return nil, errMessageCursor
			}
		default:
			return nil, errMessageCursor
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') || len(seen) != 4 {
		return nil, errMessageCursor
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errMessageCursor
	}
	at, err := parseMessageTime(created)
	if err != nil {
		return nil, errMessageCursor
	}
	return &storage.MessageCursor{CreatedAt: at, ID: id}, nil
}

func encodeMessageCursor(sessionID string, cursor storage.MessageCursor) (string, error) {
	if sessionID == "" || !utf8.ValidString(sessionID) || !asciiMessageID(cursor.ID) {
		return "", errMessageCursor
	}
	created, err := formatMessageTime(cursor.CreatedAt)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(messageCursorView{Version: 1, SessionID: sessionID, CreatedAt: created, ID: cursor.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
