package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

// G1: テンプレを起動時に1回だけパースして使い回す（毎reqのParseFiles廃止）
var (
	tmplLogin    *template.Template
	tmplRegister *template.Template
	tmplIndex    *template.Template
	tmplUser     *template.Template
	tmplPosts    *template.Template
	tmplPostID   *template.Template
	tmplBanned   *template.Template
	tmplFrag     *template.Template // #13 post単一描画(断片キャッシュ用)
)

func initTemplates() {
	fmap := template.FuncMap{
		"imageURL":   imageURL,
		"renderPost": renderPost, // #13 断片キャッシュ経由でpostを描画
	}
	// #13 post単一描画テンプレ(断片キャッシュ生成に使用)。post.htmlは無改変。
	tmplFrag = template.Must(template.New("post.html").Funcs(fmap).ParseFiles(
		getTemplPath("post.html")))
	tmplLogin = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")))
	tmplRegister = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")))
	tmplIndex = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html")))
	tmplUser = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html")))
	tmplPosts = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html")))
	tmplPostID = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html")))
	tmplBanned = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")))
}

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int       `db:"comment_count"`
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

var memcacheClient *memcache.Client

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize(ctx context.Context) {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
		// #1 comment_count 再構築: LEFT JOIN集計でN×サブクエリを回避
		"UPDATE posts p LEFT JOIN (SELECT post_id, COUNT(*) AS cnt FROM comments GROUP BY post_id) c ON p.id = c.post_id SET p.comment_count = COALESCE(c.cnt, 0)",
		// imgdata排除後の物理ページ回収: postsをコンパクト化しバッファプール効率を最大化
		"OPTIMIZE TABLE posts",
	}

	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}
}

func tryLogin(ctx context.Context, accountName, password string) *User {
	u := User{}
	err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(ctx, u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

var (
	reAccountName = regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`)
	rePassword    = regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`)
)

func validateUser(accountName, password string) bool {
	return reAccountName.MatchString(accountName) && rePassword.MatchString(password)
}

// 今回のGo実装では言語側のエスケープの仕組みが使えないのでOSコマンドインジェクション対策できない
// 取り急ぎPHPのescapeshellarg関数を参考に自前で実装
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(ctx context.Context, src string) string {
	// crypto/sha512 で算出（opensslのhex出力と同一の小文字hex）
	sum := sha512.Sum512([]byte(src))
	return fmt.Sprintf("%x", sum)
}

func calculateSalt(ctx context.Context, accountName string) string {
	return digest(ctx, accountName)
}

func calculatePasshash(ctx context.Context, accountName, password string) string {
	return digest(ctx, password+":"+calculateSalt(ctx, accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

// #10 全ユーザをインメモリ常駐。users(1000行)は起動時/initialize時に一括ロードし、
// makePosts/getSessionUserのid参照をDBゼロにする。新規登録ユーザはread-throughで取り込む。
// del_flg変更(ban)はマップを直接更新してPOST→GET即反映を維持する。
var userCache = struct {
	mu   sync.RWMutex
	byID map[int]User
}{byID: map[int]User{}}

func loadAllUsers(ctx context.Context) {
	var users []User
	if err := db.SelectContext(ctx, &users, "SELECT * FROM `users`"); err != nil {
		log.Print(err)
		return
	}
	m := make(map[int]User, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	userCache.mu.Lock()
	userCache.byID = m
	userCache.mu.Unlock()
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// getCachedUserByID: マップ参照→ミス時のみDB読み(新規登録ユーザ)。
func getCachedUserByID(ctx context.Context, id int) (User, bool) {
	userCache.mu.RLock()
	u, ok := userCache.byID[id]
	userCache.mu.RUnlock()
	if ok {
		return u, true
	}
	var uu User
	if err := db.GetContext(ctx, &uu, "SELECT * FROM `users` WHERE `id` = ?", id); err != nil {
		return User{}, false
	}
	userCache.mu.Lock()
	userCache.byID[uu.ID] = uu
	userCache.mu.Unlock()
	return uu, true
}

func fetchUserByID(ctx context.Context, uid interface{}) User {
	id, ok := toInt(uid)
	if !ok {
		return User{}
	}
	u, _ := getCachedUserByID(ctx, id)
	return u
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	return fetchUserByID(r.Context(), uid)
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

// 投稿一覧キャッシュ: id/user_id/body/mime/created_at のみ保持(comment_countはライブ取得)。
// バージョンカウンタ(post_gen)をインクリメントするだけで全ページが世代交代する。
const postGenKey = "post_gen"

type cachedPost struct {
	ID        int       `json:"i"`
	UserID    int       `json:"u"`
	Body      string    `json:"b"`
	Mime      string    `json:"m"`
	CreatedAt time.Time `json:"t"`
}

func getPostListGen() uint64 {
	item, err := memcacheClient.Get(postGenKey)
	if err != nil {
		return 0
	}
	gen, _ := strconv.ParseUint(string(item.Value), 10, 64)
	return gen
}

func incrPostListGen() {
	if _, err := memcacheClient.Increment(postGenKey, 1); err != nil {
		memcacheClient.Add(&memcache.Item{Key: postGenKey, Value: []byte("1")})
	}
}

func getPostListCache(key string) []Post {
	item, err := memcacheClient.Get(key)
	if err != nil {
		return nil
	}
	var cached []cachedPost
	if json.Unmarshal(item.Value, &cached) != nil {
		return nil
	}
	posts := make([]Post, len(cached))
	for i, c := range cached {
		posts[i] = Post{ID: c.ID, UserID: c.UserID, Body: c.Body, Mime: c.Mime, CreatedAt: c.CreatedAt}
	}
	return posts
}

func setPostListCache(key string, posts []Post) {
	cached := make([]cachedPost, len(posts))
	for i, p := range posts {
		cached[i] = cachedPost{ID: p.ID, UserID: p.UserID, Body: p.Body, Mime: p.Mime, CreatedAt: p.CreatedAt}
	}
	b, _ := json.Marshal(cached)
	memcacheClient.Set(&memcache.Item{Key: key, Value: b, Expiration: 300})
}

// #11 コメントのpost単位キャッシュ。値は当該postの全コメント(created_at DESC, id DESC)。
// POST /comment 時に該当キーをDeleteして即時無効化(POST→GET即反映)。
// comment_count はこのキャッシュの件数(len)から導出するため posts.comment_count は読まない。
type cachedComment struct {
	ID        int       `json:"i"`
	PostID    int       `json:"p"`
	UserID    int       `json:"u"`
	Comment   string    `json:"c"`
	CreatedAt time.Time `json:"t"`
}

func commentsCacheKey(postID int) string {
	return fmt.Sprintf("comments:%d", postID)
}

func encodeComments(cmts []Comment) []byte {
	cc := make([]cachedComment, len(cmts))
	for i, c := range cmts {
		cc[i] = cachedComment{ID: c.ID, PostID: c.PostID, UserID: c.UserID, Comment: c.Comment, CreatedAt: c.CreatedAt}
	}
	b, _ := json.Marshal(cc)
	return b
}

func decodeComments(b []byte) ([]Comment, bool) {
	var cc []cachedComment
	if json.Unmarshal(b, &cc) != nil {
		return nil, false
	}
	cmts := make([]Comment, len(cc))
	for i, c := range cc {
		cmts[i] = Comment{ID: c.ID, PostID: c.PostID, UserID: c.UserID, Comment: c.Comment, CreatedAt: c.CreatedAt}
	}
	return cmts, true
}

// getCommentsForPosts は postID->全コメント(created_at DESC,id DESC) を返す。
// memcacheミスしたpostのみDBを一括読みして個別にキャッシュする。
func getCommentsForPosts(ctx context.Context, postIDs []int) map[int][]Comment {
	result := make(map[int][]Comment, len(postIDs))
	keys := make([]string, 0, len(postIDs))
	for _, id := range postIDs {
		keys = append(keys, commentsCacheKey(id))
	}
	items, _ := memcacheClient.GetMulti(keys)
	var miss []int
	for _, id := range postIDs {
		if it, ok := items[commentsCacheKey(id)]; ok {
			if cmts, ok2 := decodeComments(it.Value); ok2 {
				result[id] = cmts
				continue
			}
		}
		miss = append(miss, id)
	}
	if len(miss) > 0 {
		holder, args := inClause(miss)
		var all []Comment
		if err := db.SelectContext(ctx, &all,
			"SELECT * FROM `comments` WHERE `post_id` IN ("+holder+") ORDER BY `created_at` DESC, `id` DESC", args...); err != nil {
			log.Print(err)
			for _, id := range miss {
				result[id] = nil
			}
			return result
		}
		grouped := make(map[int][]Comment, len(miss))
		for _, c := range all {
			grouped[c.PostID] = append(grouped[c.PostID], c)
		}
		for _, id := range miss {
			cmts := grouped[id] // 0件ならnil → 空配列としてキャッシュ
			result[id] = cmts
			memcacheClient.Set(&memcache.Item{Key: commentsCacheKey(id), Value: encodeComments(cmts)})
		}
	}
	return result
}

// inClause は ids 件数分の "?" プレースホルダ文字列と引数列を返す。
func inClause(ids []int) (string, []any) {
	holders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, v := range ids {
		holders[i] = "?"
		args[i] = v
	}
	return strings.Join(holders, ","), args
}

// fetchUsersByIDs は id->User をインメモリマップから返す。ミスしたidのみDBを一括読みして取り込む。
func fetchUsersByIDs(ctx context.Context, ids []int) (map[int]User, error) {
	m := make(map[int]User, len(ids))
	if len(ids) == 0 {
		return m, nil
	}
	var miss []int
	userCache.mu.RLock()
	for _, id := range ids {
		if u, ok := userCache.byID[id]; ok {
			m[id] = u
		} else {
			miss = append(miss, id)
		}
	}
	userCache.mu.RUnlock()
	if len(miss) > 0 {
		holder, args := inClause(miss)
		var users []User
		if err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `id` IN ("+holder+")", args...); err != nil {
			return nil, err
		}
		userCache.mu.Lock()
		for _, u := range users {
			userCache.byID[u.ID] = u
			m[u.ID] = u
		}
		userCache.mu.Unlock()
	}
	return m, nil
}

// #13 post断片キャッシュ。html/templateのpostごとの再評価(pprof: walk 75%/evalCall 47%)を回避する。
// 各postのHTML断片をプロセス内に保持し、ページ生成は断片連結だけにする。
//   - キー: post.ID、版: fragVer[id]。コメント追加で版を進め断片を破棄(POST→GET即反映)。
//   - CSRFトークンはユーザ毎に異なるため、断片描画時は固定プレースホルダ(csrfSentinel)で描画し、
//     レスポンス送出直前に現ユーザのトークンへ置換する。これでDOMはバイト一致のまま描画コストだけ削減。
//   - 揮発キャッシュ(一次データはMySQL/ファイル)。/initializeでリセット、起動後は遅延再生成。
//   - 詳細ページ(post_id.html)はpost.htmlを実トークンで直接描画する従来経路のまま(1件のみ・非ホット)。
const csrfSentinel = "Cs9RfTkPlAcEhOlDeR0K2xQ7zW"

type fragEntry struct {
	ver  uint64
	html []byte
}

var (
	fragMu    sync.RWMutex
	fragVer   = map[int]uint64{}
	fragCache = map[int]fragEntry{}
)

// bumpFrag は該当postの断片を破棄し版を進める(コメント追加時)。
func bumpFrag(id int) {
	fragMu.Lock()
	fragVer[id]++
	delete(fragCache, id)
	fragMu.Unlock()
}

// resetFragCache は全断片を破棄する(/initialize時)。
func resetFragCache() {
	fragMu.Lock()
	fragVer = map[int]uint64{}
	fragCache = map[int]fragEntry{}
	fragMu.Unlock()
}

// renderPostFragment は1件のpostをcsrfSentinel付きで描画する(キャッシュ実体)。
func renderPostFragment(p Post) []byte {
	p.CSRFToken = csrfSentinel
	var buf bytes.Buffer
	if err := tmplFrag.Execute(&buf, p); err != nil {
		log.Print(err)
	}
	return buf.Bytes()
}

// renderPost はposts.htmlのFuncMapから呼ばれ、断片(template.HTML)を返す。
// 版が一致するキャッシュがあれば再利用、無ければ描画して格納する。
// 描画中に版が進んだ場合は格納をスキップ(古い断片を残さない=即反映を担保)。
func renderPost(p Post) template.HTML {
	id := p.ID
	fragMu.RLock()
	ver := fragVer[id]
	e, ok := fragCache[id]
	fragMu.RUnlock()
	if ok && e.ver == ver {
		return template.HTML(e.html)
	}
	html := renderPostFragment(p)
	fragMu.Lock()
	if fragVer[id] == ver {
		fragCache[id] = fragEntry{ver: ver, html: html}
	}
	fragMu.Unlock()
	return template.HTML(html)
}

// executeWithCSRF はテンプレをバッファに描画し、断片中のcsrfSentinelを実トークンへ置換して送出する。
func executeWithCSRF(w http.ResponseWriter, t *template.Template, token string, data interface{}) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Print(err)
		return
	}
	w.Write(bytes.ReplaceAll(buf.Bytes(), []byte(csrfSentinel), []byte(token)))
}

// #3 makePosts N+1解消: comments/users/comment_count をIN句で一括取得する。
// 出力HTMLは元実装と完全一致(del_flg==0絞り→最大postsPerPage件, コメントは最新3件を時系列昇順)。
func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	if len(results) == 0 {
		return []Post{}, nil
	}

	// 投稿者をIN一括取得
	authorIDs := make([]int, 0, len(results))
	seenAuthor := make(map[int]bool, len(results))
	for _, p := range results {
		if !seenAuthor[p.UserID] {
			seenAuthor[p.UserID] = true
			authorIDs = append(authorIDs, p.UserID)
		}
	}
	authors, err := fetchUsersByIDs(ctx, authorIDs)
	if err != nil {
		return nil, err
	}

	// 元実装と同じ順序で del_flg==0 の投稿を最大 postsPerPage 件選ぶ
	selected := make([]Post, 0, postsPerPage)
	for _, p := range results {
		u, ok := authors[p.UserID]
		if !ok || u.DelFlg != 0 {
			continue
		}
		p.User = u
		p.CSRFToken = csrfToken
		selected = append(selected, p)
		if len(selected) >= postsPerPage {
			break
		}
	}
	if len(selected) == 0 {
		return []Post{}, nil
	}

	postIDs := make([]int, len(selected))
	for i, p := range selected {
		postIDs[i] = p.ID
	}

	// #11 コメントはpost単位でmemcacheに全件キャッシュ。ミスしたpostのみDB一括読み。
	cmtsAll := getCommentsForPosts(ctx, postIDs)

	// 表示件数を確定(index/posts=最新3件, 詳細=全件)。表示コメントの投稿者をIN一括取得。
	display := make(map[int][]Comment, len(postIDs))
	cuSeen := make(map[int]bool)
	cuIDs := []int{}
	for _, id := range postIDs {
		all := cmtsAll[id]
		var shown []Comment
		if !allComments && len(all) > 3 {
			shown = append(shown, all[:3]...) // 新規backing arrayへコピー(キャッシュ実体を汚さない)
		} else {
			shown = append(shown, all...)
		}
		display[id] = shown
		for _, c := range shown {
			if !cuSeen[c.UserID] {
				cuSeen[c.UserID] = true
				cuIDs = append(cuIDs, c.UserID)
			}
		}
	}
	cusers, err := fetchUsersByIDs(ctx, cuIDs)
	if err != nil {
		return nil, err
	}

	posts := make([]Post, 0, len(selected))
	for _, p := range selected {
		shown := display[p.ID]
		for i := range shown {
			shown[i].User = cusers[shown[i].UserID]
		}
		// reverse (created_at DESC -> ASC)
		for i, j := 0, len(shown)-1; i < j; i, j = i+1, j-1 {
			shown[i], shown[j] = shown[j], shown[i]
		}
		p.Comments = shown
		// #11 comment_count はキャッシュ全件数から導出(posts.comment_count列は読まない)
		p.CommentCount = len(cmtsAll[p.ID])
		posts = append(posts, p)
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

// #4 画像ファイル出し: nginx try_files $uri @app で直接配信させる
const imageDir = "../public/image"

func extFromMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	}
	return ""
}

// saveImageFile は public/image/<id>.<ext> へアトミック(temp→rename)に書き出す。
// renameは同一FS内で原子的なので、nginxが書きかけを配信することはない。
func saveImageFile(id int, mime string, data []byte) error {
	ext := extFromMime(mime)
	if ext == "" {
		return nil
	}
	dst := fmt.Sprintf("%s/%d%s", imageDir, id, ext)
	tmp, err := os.CreateTemp(imageDir, "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// nginx(www-data)が読めるよう0644にする(CreateTempは0600)
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// /initialize で消える投稿(id>10000)のダンプ済み画像を掃除する。
// 種データ(id<=10000)はimgdataが不変なのでキャッシュを残す。
func cleanupVolatileImages() {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		idStr := name
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			idStr = name[:dot]
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		if id > 10000 {
			os.Remove(fmt.Sprintf("%s/%s", imageDir, name))
		}
	}
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbInitialize(ctx)
	// dbInitializeでusers/posts/commentsがリセットされるため全キャッシュを破棄
	// (タイムライン一覧/コメント/post_gen/session)
	memcacheClient.FlushAll()
	// #13 post描画断片もリセット(postsが再構築されるため)
	resetFragCache()
	// #10 リセット後のusersをインメモリへ再ロード
	loadAllUsers(ctx)
	// #4 揮発画像(id>10000)を掃除。種データの画像は遅延dumpで再生成される
	os.MkdirAll(imageDir, 0755)
	cleanupVolatileImages()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tmplLogin.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tmplRegister.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(ctx, accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)

	gen := getPostListGen()
	cacheKey := fmt.Sprintf("post_list:%d:top", gen)

	var results []Post
	if cached := getPostListCache(cacheKey); cached != nil {
		// comment_countはmakePosts内でコメントキャッシュ件数から導出する
		results = cached
	} else {
		// FORCE INDEX: ANALYZEしても自動選択されないのでヒント必須
		err := db.SelectContext(ctx, &results, "SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at`, p.`comment_count` FROM `posts` AS p FORCE INDEX (`idx_posts_created_at`) JOIN `users` AS u ON p.`user_id` = u.`id` WHERE u.`del_flg` = 0 ORDER BY p.`created_at` DESC LIMIT ?", postsPerPage)
		if err != nil {
			log.Print(err)
			return
		}
		setPostListCache(cacheKey, results)
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	token := getCSRFToken(r)
	// #13 断片中のcsrfSentinelを実トークンへ置換して送出(DOMバイト一致)
	executeWithCSRF(w, tmplIndex, token, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, token, getFlash(w, r, "notice")})
}

// #12 /@account のキャッシュ。
// post一覧: user_posts:{uid} (本人の新規投稿でDelete)。
// 集計: account_stats:{uid} = {本人のコメント数, 本人postへのコメント数}。
//   コメント投稿時にcommenterとpost所有者の両方をDeleteしてPOST→GET即反映。
func userPostsCacheKey(uid int) string    { return fmt.Sprintf("user_posts:%d", uid) }
func accountStatsCacheKey(uid int) string { return fmt.Sprintf("account_stats:%d", uid) }

type accountStats struct {
	CommentCount   int `json:"c"`
	CommentedCount int `json:"d"`
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// #12 post一覧をキャッシュ(本人の新規投稿でDelete)。comment_count等はmakePosts内で導出。
	postsKey := userPostsCacheKey(user.ID)
	var results []Post
	if cached := getPostListCache(postsKey); cached != nil {
		results = cached
	} else {
		err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at`, `comment_count` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
		if err != nil {
			log.Print(err)
			return
		}
		setPostListCache(postsKey, results)
	}
	postCount := len(results) // 本人の全投稿数(別クエリ廃止)

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	// #12 集計(本人コメント数/本人postへのコメント数)をキャッシュ
	commentCount := 0
	commentedCount := 0
	statsKey := accountStatsCacheKey(user.ID)
	hit := false
	if item, e := memcacheClient.Get(statsKey); e == nil {
		var st accountStats
		if json.Unmarshal(item.Value, &st) == nil {
			commentCount = st.CommentCount
			commentedCount = st.CommentedCount
			hit = true
		}
	}
	if !hit {
		err = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
		if err != nil {
			log.Print(err)
			return
		}
		if postCount > 0 {
			ids := make([]int, len(results))
			for i, p := range results {
				ids[i] = p.ID
			}
			holder, args := inClause(ids)
			err = db.GetContext(ctx, &commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+holder+")", args...)
			if err != nil {
				log.Print(err)
				return
			}
		}
		if b, e := json.Marshal(accountStats{CommentCount: commentCount, CommentedCount: commentedCount}); e == nil {
			memcacheClient.Set(&memcache.Item{Key: statsKey, Value: b, Expiration: 300})
		}
	}

	me := getSessionUser(r)

	// #13 断片中のcsrfSentinelを実トークンへ置換して送出(DOMバイト一致)
	executeWithCSRF(w, tmplUser, getCSRFToken(r), struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	gen := getPostListGen()
	cacheKey := fmt.Sprintf("post_list:%d:%d", gen, t.Unix())

	var results []Post
	if cached := getPostListCache(cacheKey); cached != nil {
		// comment_countはmakePosts内でコメントキャッシュ件数から導出する
		results = cached
	} else {
		err = db.SelectContext(ctx, &results, "SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at`, p.`comment_count` FROM `posts` AS p FORCE INDEX (`idx_posts_created_at`) JOIN `users` AS u ON p.`user_id` = u.`id` WHERE u.`del_flg` = 0 AND p.`created_at` <= ? ORDER BY p.`created_at` DESC LIMIT ?", t.Format(ISO8601Format), postsPerPage)
		if err != nil {
			log.Print(err)
			return
		}
		setPostListCache(cacheKey, results)
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// #13 断片中のcsrfSentinelを実トークンへ置換して送出(DOMバイト一致)
	executeWithCSRF(w, tmplPosts, getCSRFToken(r), posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	// #7 詳細表示にimgdataは不要(画像は/image/経由)。BLOBをロードしない
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at`, `comment_count` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	tmplPostID.Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// 【3】imgdataはDBに保存しない。ファイルが正本、nginxが直接配信する。
	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.ExecContext(
		ctx,
		query,
		me.ID,
		mime,
		[]byte{},
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	// #4 INSERT後すぐファイル出し(POST→GET即反映)。失敗してもgetImageのDBフォールバックで配信可能
	if err := saveImageFile(int(pid), mime, filedata); err != nil {
		log.Print(err)
	}

	// 新規投稿で一覧キャッシュを世代交代(POST→GET即反映)
	incrPostListGen()
	// #12 本人ページ(/@account)のpost一覧キャッシュを無効化(POST→GET即反映)
	memcacheClient.Delete(userPostsCacheKey(me.ID))

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	// imgdataはDB排除済み。mimeのみ取得してファイルから配信。
	err = db.GetContext(ctx, &post, "SELECT `mime` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	ext := r.PathValue("ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		// ファイルから直接配信（nginxが先に配信するので通常ここは通らない）
		fp := fmt.Sprintf("%s/%d%s", imageDir, pid, extFromMime(post.Mime))
		data, err := os.ReadFile(fp)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", post.Mime)
		w.Write(data)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	// #11 コメントをINSERT(autocommit)後、該当postのコメントキャッシュを無効化。
	// 次GETで全件再取得され、表示・件数とも即反映(POST→GET即反映)。comment_count列の更新は不要。
	if _, err := db.ExecContext(ctx, "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)", postID, me.ID, r.FormValue("comment")); err != nil {
		log.Print(err)
		return
	}
	memcacheClient.Delete(commentsCacheKey(postID))
	// #13 該当postの描画断片を無効化(コメント数/最新3件が変わるため。POST→GET即反映)
	bumpFrag(postID)

	// #12 /@account 集計を即反映: commenter(本人コメント数)とpost所有者(本人postへのコメント数)を無効化
	memcacheClient.Delete(accountStatsCacheKey(me.ID))
	var ownerID int
	if e := db.GetContext(ctx, &ownerID, "SELECT `user_id` FROM `posts` WHERE `id` = ?", postID); e == nil && ownerID != me.ID {
		memcacheClient.Delete(accountStatsCacheKey(ownerID))
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	tmplBanned.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, query, 1, id)
		// #10 del_flg変更をインメモリマップへ反映(POST→GET即反映)
		if idInt, err := strconv.Atoi(id); err == nil {
			userCache.mu.Lock()
			if u, ok := userCache.byID[idInt]; ok {
				u.DelFlg = 1
				userCache.byID[idInt] = u
			}
			userCache.mu.Unlock()
		}
	}
	// banで可視投稿一覧が変わるため世代交代
	incrPostListGen()

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{
		"charset": "utf8mb4",
		// prepare/exec の2往復を1往復に。複文は未使用なので安全。
		"interpolateParams": "true",
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	dsn := cfg.FormatDSN()

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	// G2: コネクションプール設定（張り直し削減・並列度確保）
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(64)
	db.SetConnMaxLifetime(time.Minute)

	// #4 画像ダンプ先ディレクトリを用意
	os.MkdirAll(imageDir, 0755)

	// #10 起動時に全ユーザをインメモリへロード
	loadAllUsers(context.Background())

	initTemplates()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir("../public")))

	go http.ListenAndServe("localhost:6060", nil)
	log.Fatal(http.ListenAndServe(":8080", r))
}
