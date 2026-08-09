package controlhttp

import (
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// HAR 1.2 export.
//
// HAR exists so a captured session opens in tools people already have: browser
// devtools, Charles, Fiddler, har-analyzer. That is worth more than a bespoke
// format nobody can read, which is why the request log gets a second
// representation rather than a second API.

type harLog struct {
	Log harLogBody `json:"log"`
}

type harLogBody struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Pages   []harPage  `json:"pages"`
	Entries []harEntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harPage struct {
	StartedDateTime string        `json:"startedDateTime"`
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	PageTimings     harPageTiming `json:"pageTimings"`
}

type harPageTiming struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
}

type harEntry struct {
	Pageref         string      `json:"pageref,omitempty"`
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           harCache    `json:"cache"`
	Timings         harTimings  `json:"timings"`
	ServerIPAddress string      `json:"serverIPAddress,omitempty"`
	Connection      string      `json:"connection,omitempty"`
	Comment         string      `json:"comment,omitempty"`
}

type harRequest struct {
	Method      string        `json:"method"`
	URL         string        `json:"url"`
	HTTPVersion string        `json:"httpVersion"`
	Cookies     []harCookie   `json:"cookies"`
	Headers     []harNameVal  `json:"headers"`
	QueryString []harNameVal  `json:"queryString"`
	PostData    *harPostData  `json:"postData,omitempty"`
	HeadersSize int64         `json:"headersSize"`
	BodySize    int64         `json:"bodySize"`
}

type harResponse struct {
	Status      int          `json:"status"`
	StatusText  string       `json:"statusText"`
	HTTPVersion string       `json:"httpVersion"`
	Cookies     []harCookie  `json:"cookies"`
	Headers     []harNameVal `json:"headers"`
	Content     harContent   `json:"content"`
	RedirectURL string       `json:"redirectURL"`
	HeadersSize int64        `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
}

type harContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Comment  string `json:"comment,omitempty"`

	// Fields prefixed with an underscore are HAR's sanctioned extension
	// mechanism: readers ignore what they do not know. The digest is what makes
	// a body fetchable from /v1/artifacts later, so it belongs in the export
	// even when the bytes themselves do not.
	Digest      string `json:"_digest,omitempty"`
	ArtifactURL string `json:"_artifactUrl,omitempty"`
}

type harPostData struct {
	MimeType string       `json:"mimeType"`
	Params   []harNameVal `json:"params"`
	Text     string       `json:"text,omitempty"`
	Digest   string       `json:"_digest,omitempty"`
}

type harCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harNameVal struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harCache struct{}

type harTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// maxInlineBody bounds what ?bodies=1 will inline. A HAR with a hundred
// megabytes of captured video in it is not openable by the tools HAR exists to
// be openable by.
const maxInlineBody = 2 << 20 // 2 MiB

func (h *Handler) exportHAR(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	sess, err := h.fleet.GetSession(ctx, sessionID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	exchanges, err := h.fleet.Exchanges(ctx, sessionID, intParam(r, "limit", 5000))
	if err != nil {
		writeDomainError(w, err)
		return
	}

	inline := boolParam(r, "bodies")

	page := harPage{
		StartedDateTime: rfc3339(sess.CreatedAt),
		ID:              sessionID,
		Title:           defaultTitle(sess),
		PageTimings:     harPageTiming{OnContentLoad: -1, OnLoad: -1},
	}

	entries := make([]harEntry, 0, len(exchanges))
	for _, x := range exchanges {
		entries = append(entries, h.harEntry(r, x, sessionID, inline))
	}

	comment := "Exported by hubd. "
	if agent, err := h.fleet.GetAgent(ctx, sess.AgentID); err == nil {
		if gaps := agent.Capabilities.CaptureGaps(); len(gaps) > 0 {
			// A HAR that silently omits navigations looks like a page that made
			// no navigation requests. Saying so in the file itself is the only
			// place the warning survives being downloaded and mailed around.
			comment += "This capture is not complete: " + strings.Join(gaps, "; ") + "."
		} else {
			comment += "Full-fidelity capture."
		}
	}

	w.Header().Set("Content-Disposition", `attachment; filename="`+sessionID+`.har"`)
	writeJSON(w, http.StatusOK, harLog{Log: harLogBody{
		Version: "1.2",
		Creator: harCreator{Name: "hubd", Version: h.version},
		Pages:   []harPage{page},
		Entries: entries,
		Comment: comment,
	}})
}

func (h *Handler) harEntry(r *http.Request, x core.Exchange, sessionID string, inline bool) harEntry {
	req := harRequest{
		Method:      defaultString(x.Method, "GET"),
		URL:         x.URL,
		HTTPVersion: "HTTP/1.1",
		Cookies:     []harCookie{},
		Headers:     headerList(x.ReqHeaders),
		QueryString: queryList(x.URL),
		HeadersSize: -1,
		BodySize:    x.ReqBodySize,
	}
	if x.ReqBody != "" {
		req.PostData = &harPostData{
			MimeType: headerValue(x.ReqHeaders, "content-type"),
			Params:   []harNameVal{},
			Digest:   x.ReqBody,
		}
		if inline {
			req.PostData.Text = h.inlineBody(r, x.ReqBody)
		}
	}

	content := harContent{
		Size:     x.ResBodySize,
		MimeType: x.MimeType,
	}
	if x.ResBody != "" {
		content.Digest = x.ResBody
		content.ArtifactURL = "/v1/artifacts/" + x.ResBody
		if inline {
			content.Text = h.inlineBody(r, x.ResBody)
		}
	}
	if x.Truncated {
		content.Comment = "the agent truncated this body at its capture cap"
	}

	return harEntry{
		Pageref:         sessionID,
		StartedDateTime: rfc3339(x.StartedAt),
		Time:            float64(x.DurationMs),
		Request:         req,
		Response: harResponse{
			Status:      x.Status,
			StatusText:  x.StatusText,
			HTTPVersion: "HTTP/1.1",
			Cookies:     []harCookie{},
			Headers:     headerList(x.ResHeaders),
			Content:     content,
			RedirectURL: headerValue(x.ResHeaders, "location"),
			HeadersSize: -1,
			BodySize:    x.ResBodySize,
		},
		Cache: harCache{},
		// -1 is HAR's "not measured". Inventing plausible numbers here would be
		// worse than admitting the agent did not report them.
		Timings: harTimings{Send: -1, Wait: -1, Receive: float64(x.DurationMs)},
		Comment: cacheComment(x),
	}
}

// inlineBody fetches a stored artifact for embedding, within the size cap.
func (h *Handler) inlineBody(r *http.Request, digest string) string {
	if h.blobs == nil {
		return ""
	}
	rc, info, err := h.blobs.Open(r.Context(), digest)
	if err != nil {
		return ""
	}
	defer rc.Close()
	if info.Size > maxInlineBody {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxInlineBody))
	if err != nil {
		return ""
	}
	return string(b)
}

func cacheComment(x core.Exchange) string {
	if x.FromCache {
		return "served from the browser cache"
	}
	return ""
}

func defaultTitle(s core.Session) string {
	if s.Title != "" {
		return s.Title
	}
	if s.URL != "" {
		return s.URL
	}
	return s.ID
}

// headerList converts a header map to HAR's ordered name/value pairs.
//
// Sorted, because Go map iteration is randomized and two exports of the same
// session should diff cleanly against each other.
func headerList(m map[string]string) []harNameVal {
	out := make([]harNameVal, 0, len(m))
	for k, v := range m {
		out = append(out, harNameVal{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func headerValue(m map[string]string, name string) string {
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func queryList(rawURL string) []harNameVal {
	u, err := url.Parse(rawURL)
	if err != nil {
		return []harNameVal{}
	}
	q := u.Query()
	out := make([]harNameVal, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			out = append(out, harNameVal{Name: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
