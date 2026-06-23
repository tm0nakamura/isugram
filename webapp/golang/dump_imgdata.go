//go:build ignore

// dump_imgdata: 種データ(id<=10000)のimgdataをファイルへdumpし、DBを空化する。
// 実行: go run dump_imgdata.go
// 一度だけ実行。実行後はgetImageがDB imgdataを読まない前提になる。
package main

import (
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func main() {
	db, err := sqlx.Open("mysql", "isuconp:isuconp@tcp(localhost:3306)/isuconp?parseTime=true")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	imageDir := "/home/isucon/private_isu/webapp/public/image"
	os.MkdirAll(imageDir, 0755)

	extMap := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
	}

	type Row struct {
		ID      int    `db:"id"`
		Mime    string `db:"mime"`
		Imgdata []byte `db:"imgdata"`
	}

	fmt.Println("Fetching seed images from DB...")
	var rows []Row
	if err := db.Select(&rows, "SELECT id, mime, imgdata FROM posts WHERE id <= 10000 AND LENGTH(imgdata) > 0"); err != nil {
		panic(err)
	}
	fmt.Printf("Found %d images to dump\n", len(rows))

	dumped := 0
	skipped := 0
	for i, r := range rows {
		ext := extMap[r.Mime]
		if ext == "" {
			continue
		}
		dst := fmt.Sprintf("%s/%d%s", imageDir, r.ID, ext)
		if _, err := os.Stat(dst); err == nil {
			skipped++
			continue
		}
		if err := os.WriteFile(dst, r.Imgdata, 0644); err != nil {
			fmt.Printf("ERROR writing %s: %v\n", dst, err)
			continue
		}
		dumped++
		if (i+1)%500 == 0 {
			fmt.Printf("  progress: %d/%d\n", i+1, len(rows))
		}
	}
	fmt.Printf("Dump done: %d written, %d skipped (already existed)\n", dumped, skipped)

	fmt.Println("Clearing imgdata from DB (all posts)...")
	result, err := db.Exec("UPDATE posts SET imgdata = ''")
	if err != nil {
		panic(err)
	}
	n, _ := result.RowsAffected()
	fmt.Printf("imgdata cleared: %d rows\n", n)
	fmt.Println("Complete. posts table is now BLOB-free.")
}
