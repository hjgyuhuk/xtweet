package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed static/index.html
var staticFiles embed.FS

// ── Token ────────────────────────────────────────────────────────────────────

func computeToken(id string) string {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return ""
	}
	val := (float64(n) / 1e15) * math.Pi
	b36 := floatToBase36(val)
	var sb strings.Builder
	for _, c := range b36 {
		if c != '0' && c != '.' {
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

func floatToBase36(f float64) string {
	const alpha = "0123456789abcdefghijklmnopqrstuvwxyz"
	if f < 0 {
		return "-" + floatToBase36(-f)
	}
	intPart := int64(f)
	fracPart := f - float64(intPart)
	var iRunes []rune
	if intPart == 0 {
		iRunes = []rune{'0'}
	} else {
		for n := intPart; n > 0; n /= 36 {
			iRunes = append([]rune{rune(alpha[n%36])}, iRunes...)
		}
	}
	if fracPart < 1e-14 {
		return string(iRunes)
	}
	var fRunes []rune
	for i := 0; i < 20 && fracPart > 1e-14; i++ {
		fracPart *= 36
		d := int(fracPart)
		fRunes = append(fRunes, rune(alpha[d]))
		fracPart -= float64(d)
	}
	return string(iRunes) + "." + string(fRunes)
}

// ── ID 提取 ──────────────────────────────────────────────────────────────────

var statusRe = regexp.MustCompile(`(?:twitter\.com|x\.com)/[^/]+/status/(\d+)`)

func extractID(raw string) (string, error) {
	if m := statusRe.FindStringSubmatch(raw); len(m) >= 2 {
		return m[1], nil
	}
	if ok, _ := regexp.MatchString(`^\d{10,}$`, strings.TrimSpace(raw)); ok {
		return strings.TrimSpace(raw), nil
	}
	return "", fmt.Errorf("无法识别推文链接或 ID: %q", raw)
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 15 * time.Second}

func get(url string, headers map[string]string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.StatusCode, err
}

// ── 数据源 1：syndication ─────────────────────────────────────────────────────

func fetchSyndication(id string) ([]byte, error) {
	u := fmt.Sprintf(
		"https://cdn.syndication.twimg.com/tweet-result?id=%s&lang=zh&token=%s",
		id, computeToken(id),
	)
	body, status, err := get(u, map[string]string{
		"User-Agent": "Mozilla/5.0 (compatible; xtweet/1.0)",
		"Referer":    "https://platform.twitter.com/",
	})
	if err != nil {
		return nil, fmt.Errorf("syndication 请求失败: %w", err)
	}
	switch status {
	case 200:
	case 404:
		return nil, fmt.Errorf("推文不存在或已被删除")
	case 429:
		return nil, fmt.Errorf("Twitter API 限流，请稍后重试")
	default:
		return nil, fmt.Errorf("Twitter API 返回 %d", status)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("syndication 返回非法 JSON")
	}
	return body, nil
}

// isNoteTweet 检测长推文：note_tweet 字段存在且不为空对象
func isNoteTweet(body []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}
	raw, ok := top["note_tweet"]
	return ok && string(raw) != "null" && len(raw) > 2
}

// ── 数据源 2：fxtwitter ───────────────────────────────────────────────────────
// 结构根据实际 API 响应对准（2026-03）

type fxResponse struct {
	Code  int `json:"code"`
	Tweet struct {
		ID               string `json:"id"`
		Text             string `json:"text"`
		CreatedTimestamp int64  `json:"created_timestamp"` // unix 秒
		Lang             string `json:"lang"`
		Likes            int    `json:"likes"`
		Replies          int    `json:"replies"`
		ReplyToUser      string `json:"replying_to"` // screen_name，可为 null → ""
		Author           struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			ScreenName string `json:"screen_name"`
			AvatarURL  string `json:"avatar_url"`
		} `json:"author"`
		Media struct {
			Photos []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"photos"`
			All []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"all"`
		} `json:"media"`
		Quote *struct {
			Text   string `json:"text"`
			Author struct {
				Name       string `json:"name"`
				ScreenName string `json:"screen_name"`
				AvatarURL  string `json:"avatar_url"`
			} `json:"author"`
		} `json:"quote"`
	} `json:"tweet"`
}

func fetchFxTwitter(id string) ([]byte, error) {
	body, status, err := get("https://api.fxtwitter.com/status/"+id, map[string]string{
		"User-Agent": "xtweet/1.0",
	})
	if err != nil {
		return nil, fmt.Errorf("fxtwitter 请求失败: %w", err)
	}
	if status == 404 {
		return nil, fmt.Errorf("推文不存在或已被删除")
	}
	if status != 200 {
		return nil, fmt.Errorf("fxtwitter 返回 %d", status)
	}

	var fx fxResponse
	if err := json.Unmarshal(body, &fx); err != nil {
		return nil, fmt.Errorf("fxtwitter 解析失败: %w", err)
	}
	t := fx.Tweet
	if t.Text == "" {
		return nil, fmt.Errorf("fxtwitter 返回空内容")
	}

	createdAt := ""
	if t.CreatedTimestamp > 0 {
		createdAt = time.Unix(t.CreatedTimestamp, 0).UTC().Format(time.RFC3339)
	}

	// 转换为 syndication 兼容格式
	type synUser struct {
		IDStr                string `json:"id_str"`
		Name                 string `json:"name"`
		ScreenName           string `json:"screen_name"`
		ProfileImageURLHTTPS string `json:"profile_image_url_https"`
		IsBlueVerified       bool   `json:"is_blue_verified"`
	}
	type synMedia struct {
		MediaURLHTTPS string `json:"media_url_https"`
		Type          string `json:"type"`
	}
	type synPhoto struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	type synQuoted struct {
		Text string `json:"text"`
		User struct {
			Name                 string `json:"name"`
			ScreenName           string `json:"screen_name"`
			ProfileImageURLHTTPS string `json:"profile_image_url_https"`
		} `json:"user"`
	}
	type synTweet struct {
		Typename          string     `json:"__typename"`
		IDStr             string     `json:"id_str"`
		Text              string     `json:"text"`
		FullText          string     `json:"full_text"`
		Lang              string     `json:"lang"`
		CreatedAt         string     `json:"created_at"`
		FavoriteCount     int        `json:"favorite_count"`
		ConversationCount int        `json:"conversation_count"`
		User              synUser    `json:"user"`
		MediaDetails      []synMedia `json:"mediaDetails"`
		Photos            []synPhoto `json:"photos"`
		InReplyTo         string     `json:"in_reply_to_screen_name"`
		QuotedTweet       *synQuoted `json:"quoted_tweet"`
		Source            string     `json:"_source"`
	}

	syn := synTweet{
		Typename:          "Tweet",
		IDStr:             id,
		Text:              t.Text,
		FullText:          t.Text,
		Lang:              t.Lang,
		CreatedAt:         createdAt,
		FavoriteCount:     t.Likes,
		ConversationCount: t.Replies,
		InReplyTo:         t.ReplyToUser,
		Source:            "fxtwitter",
		User: synUser{
			IDStr:                t.Author.ID,
			Name:                 t.Author.Name,
			ScreenName:           t.Author.ScreenName,
			ProfileImageURLHTTPS: t.Author.AvatarURL,
		},
	}

	for _, p := range t.Media.Photos {
		syn.Photos = append(syn.Photos, synPhoto{URL: p.URL, Width: p.Width, Height: p.Height})
		syn.MediaDetails = append(syn.MediaDetails, synMedia{MediaURLHTTPS: p.URL, Type: "photo"})
	}
	for _, m := range t.Media.All {
		if m.Type == "video" || m.Type == "gif" {
			syn.MediaDetails = append(syn.MediaDetails, synMedia{Type: m.Type})
			break
		}
	}
	if t.Quote != nil {
		q := &synQuoted{Text: t.Quote.Text}
		q.User.Name = t.Quote.Author.Name
		q.User.ScreenName = t.Quote.Author.ScreenName
		q.User.ProfileImageURLHTTPS = t.Quote.Author.AvatarURL
		syn.QuotedTweet = q
	}

	return json.Marshal(syn)
}

// ── 主获取逻辑 ────────────────────────────────────────────────────────────────

func fetchTweetData(id string) ([]byte, error) {
	synBody, synErr := fetchSyndication(id)

	if synErr != nil {
		log.Printf("[fetch] syndication failed (%v), trying fxtwitter", synErr)
		return fetchFxTwitter(id)
	}

	if isNoteTweet(synBody) {
		log.Printf("[fetch] note_tweet detected, fetching full text via fxtwitter")
		fxBody, fxErr := fetchFxTwitter(id)
		if fxErr != nil {
			log.Printf("[fetch] fxtwitter failed (%v), falling back to truncated text", fxErr)
			return synBody, nil
		}
		return fxBody, nil
	}

	return synBody, nil
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleTweet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	raw := r.URL.Query().Get("id")
	if raw == "" {
		raw = r.URL.Query().Get("url")
	}
	if raw == "" {
		httpError(w, "缺少参数 id 或 url", http.StatusBadRequest)
		return
	}
	id, err := extractID(raw)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, err := fetchTweetData(id)
	if err != nil {
		code := http.StatusBadGateway
		if strings.Contains(err.Error(), "不存在") {
			code = http.StatusNotFound
		} else if strings.Contains(err.Error(), "限流") {
			code = http.StatusTooManyRequests
		}
		httpError(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(body)
}

// /api/raw — 返回 syndication 原始 JSON，方便调试
func handleRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	raw := r.URL.Query().Get("id")
	if raw == "" {
		httpError(w, "缺少参数 id", http.StatusBadRequest)
		return
	}
	id, err := extractID(raw)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, _, err := get(
		fmt.Sprintf("https://cdn.syndication.twimg.com/tweet-result?id=%s&lang=zh&token=%s", id, computeToken(id)),
		map[string]string{"User-Agent": "Mozilla/5.0", "Referer": "https://platform.twitter.com/"},
	)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadGateway)
		return
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		body = pretty.Bytes()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	open := flag.Bool("open", true, "启动后自动打开浏览器")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/tweet", handleTweet)
	mux.HandleFunc("/api/raw", handleRaw)

	url := "http://" + *addr
	fmt.Printf("xtweet  %s\n", url)
	if *open {
		go openBrowser(url)
	}
	log.Fatal(http.ListenAndServe(*addr, logMiddleware(mux)))
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s  %v", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	_ = os.Getenv("GOOS")
	openBrowserActual(url)
}
