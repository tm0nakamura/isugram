package main

import (
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
)

func initTemplates() {
	fmap := template.FuncMap{
		"imageURL": imageURL,
	}
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

// fetchLiveCommentCounts: キャッシュヒット時にcomment_countをDBからIN句一括取得。
func fetchLiveCommentCounts(ctx context.Context, posts []Post) {
	if len(posts) == 0 {
		return
	}
	ids := make([]int, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	holder, args := inClause(ids)
	type row struct {
		ID           int `db:"id"`
		CommentCount int `db:"comment_count"`
	}
	var rows []row
	if err := db.SelectContext(ctx, &rows, "SELECT `id`, `comment_count` FROM `posts` WHERE `id` IN ("+holder+")", args...); err != nil {
		return
	}
	cmap := make(map[int]int, len(rows))
	for _, r := range rows {
		cmap[r.ID] = r.CommentCount
	}
	for i := range posts {
		posts[i].CommentCount = cmap[posts[i].ID]
	}
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

	// #1 コメント数は posts.comment_count(非正規化)を読むだけ。GROUP BY COUNTは廃止。
	// p.CommentCount は feeding query で既に取得済み。

	// コメントを一括取得 (created_at DESC, id DESC = 単一投稿クエリ+indexと同順)
	holder, args := inClause(postIDs)
	var allCmts []Comment
	err = db.SelectContext(ctx, &allCmts,
		"SELECT * FROM `comments` WHERE `post_id` IN ("+holder+") ORDER BY `created_at` DESC, `id` DESC", args...)
	if err != nil {
		return nil, err
	}

	// post_id でグルーピング(取得順を維持)、allComments以外は3件に制限
	cmtsByPost := make(map[int][]Comment, len(postIDs))
	for _, c := range allCmts {
		if !allComments && len(cmtsByPost[c.PostID]) >= 3 {
			continue
		}
		cmtsByPost[c.PostID] = append(cmtsByPost[c.PostID], c)
	}

	// コメント投稿者をIN一括取得
	cuSeen := make(map[int]bool)
	cuIDs := []int{}
	for _, cmts := range cmtsByPost {
		for _, c := range cmts {
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
		cmts := cmtsByPost[p.ID]
		for i := range cmts {
			cmts[i].User = cusers[cmts[i].UserID]
		}
		// reverse (created_at DESC -> ASC)
		for i, j := 0, len(cmts)-1; i < j; i, j = i+1, j-1 {
			cmts[i], cmts[j] = cmts[j], cmts[i]
		}
		p.Comments = cmts
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
		results = cached
		fetchLiveCommentCounts(ctx, results)
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

	tmplIndex.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
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

	results := []Post{}

	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at`, `comment_count` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.SelectContext(ctx, &postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []any
		args := make([]any, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.GetContext(ctx, &commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	tmplUser.Execute(w, struct {
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
		results = cached
		fetchLiveCommentCounts(ctx, results)
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

	tmplPosts.Execute(w, posts)
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

	// #1 コメントINSERTとcomment_count+1を同一Txで確定(redirect前)。POST→GET即反映。
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		log.Print(err)
		return
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)", postID, me.ID, r.FormValue("comment")); err != nil {
		tx.Rollback()
		log.Print(err)
		return
	}
	if _, err = tx.ExecContext(ctx, "UPDATE `posts` SET `comment_count` = `comment_count` + 1 WHERE `id` = ?", postID); err != nil {
		tx.Rollback()
		log.Print(err)
		return
	}
	if err = tx.Commit(); err != nil {
		log.Print(err)
		return
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
