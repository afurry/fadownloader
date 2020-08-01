package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"crawshaw.io/sqlite"
	"crawshaw.io/sqlite/sqlitex"
	"github.com/headzoo/surf"
	"github.com/headzoo/surf/browser"
	"github.com/jessevdk/go-flags"
	"github.com/juju/persistent-cookiejar"
	"github.com/kirsle/configdir"
	"github.com/mitchellh/go-homedir"
	"go.uber.org/ratelimit"
	"golang.org/x/net/html"

	_ "net/http/pprof"
)

func isResponseOK(response *http.Response) bool {
	switch response.StatusCode {
	case 200:
		return true
	}
	fmt.Printf("Got response %+v\n", response)
	return false
}

func openURL(browser *browser.Browser, URL string) error {
	rl.Take()

	err := browser.Open(URL)
	if err != nil {
		return err
	}

	if !isResponseOK(browser.State().Response) {
		return fmt.Errorf("Response is not ok")
	}
	return nil
}

var opts struct {
	Help              bool   `short:"h" long:"help" description:"Display this help message"`
	ConfigDir         string `short:"c" long:"config-directory" description:"Specify config directory" value-name:"dir"`
	DownloadDirectory string `short:"d" long:"download-directory" description:"Specify download directory" value-name:"dir" default:"~/Pictures/FADownloader"`
}

var rl = ratelimit.New(3, ratelimit.WithoutSlack)
var firstTenDigits = regexp.MustCompile(`^\d{10}`)
var brokenFilename = regexp.MustCompile(`^\d{10}\.$`)

const URLbase = "https://www.furaffinity.net"

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	parser := flags.NewParser(&opts, flags.PrintErrors|flags.PassDoubleDash|flags.PassAfterNonOption)

	// set custom usage line
	parser.Usage = "[options]"

	// update parser defaults with platform/user specific values
	updateDefaults(parser)

	// parse command line options
	_, err := parser.Parse()
	if err != nil {
		panic(err)
	}

	// we do this to avoid having separate 'Help Options' section in help screen
	if opts.Help {
		parser.WriteHelp(os.Stdout)
		os.Exit(64)
	}

	if opts.ConfigDir == "" {
		log.Fatalf("Config directory is empty, that isn't acceptable")
	}
	if opts.DownloadDirectory == "" {
		log.Fatalf("Download directory is empty, that isn't acceptable")
	}

	// create browser and set it up
	mainbow := surf.NewBrowser()

	fmt.Printf("Setting cookiejar\n")
	var jar *cookiejar.Jar
	{
		cookiepath := path.Join(opts.ConfigDir, "cookies.json")
		fmt.Printf("cookie path - %s\n", cookiepath)
		jar, err = cookiejar.New(&cookiejar.Options{
			Filename: cookiepath,
		})
		if err != nil {
			panic(err)
		}
		mainbow.SetCookieJar(jar)
		mainbow.SetUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_3) AppleWebKit/536.28.10 (KHTML, like Gecko) Version/6.0.3 Safari/536.28.10")
		err = jar.Save()
		if err != nil {
			panic(err)
		}
	}
	defer func() {
		err := jar.Save()
		if err != nil {
			panic(err)
		}
	}()

	// don't keep unlimited history, we never use the feature anyway
	mainbow.HistoryJar().SetMax(1)

	fmt.Printf("Opening database...")
	dbpool, err := sqlitex.Open(path.Join(opts.ConfigDir, "downloaded.sqlite"), 0, 100)
	if err != nil {
		panic(err)
	}
	defer dbpool.Close()
	db := dbpool.Get(context.Background())
	dbMustExecute(db, "PRAGMA cache_size = 1000000")
	dbMustExecute(db, "PRAGMA temp_store = MEMORY")
	dbMustExecute(db, "PRAGMA synchronous = OFF")
	dbMustExecute(db, "PRAGMA journal_mode = WAL")
	dbMustExecute(db, "PRAGMA busy_timeout = 500000")
	dbMustExecute(db, "CREATE TABLE IF NOT EXISTS image_urls (page_url TEXT PRIMARY KEY UNIQUE, image_url TEXT, last_modified TEXT, filename TEXT)")
	dbMustExecute(db, "CREATE INDEX IF NOT EXISTS page_urls ON image_urls(page_url)")
	dbMustExecute(db, "PRAGMA optimize")
	dbMustExecute(db, "PRAGMA vacuum")
	defer dbpool.Put(db)
	fmt.Printf("\n")

	// create download directory if needed
	fmt.Printf("Creating download directory %s...", opts.DownloadDirectory)
	err = os.MkdirAll(opts.DownloadDirectory, 0700)
	if err != nil {
		log.Fatalf("Couldn't create download directory %s: %s", opts.DownloadDirectory, err)
	}
	fmt.Printf("\n")

	watchlistPage := URLbase + "/msg/submissions/"

	for {
		fmt.Printf("Loading watchlist submissions")
		err = openURL(mainbow, watchlistPage)
		if err != nil {
			fmt.Printf("Got error while getting %s: %v\n", watchlistPage, err)
			panic(err)
		}
		fmt.Printf("\n")

		// we'll be clicking on this form later
		fmt.Printf("Finding main form for images\n")
		form, err := mainbow.Form("#messages-form")
		if err != nil {
			panic(err)
		}
		form.Dom()

		inputs := mainbow.Find("#messagecenter-submissions label input")
		imageIDs := []string{}
		for _, input := range inputs.Nodes {
			attrMap := attr2map(input.Attr)
			if attrMap["type"] != "checkbox" {
				continue
			}
			if attrMap["name"] != "submissions[]" {
				continue
			}
			imageID, ok := attrMap["value"]
			if !ok {
				continue
			}
			imageIDs = append(imageIDs, imageID)
		}

		imagebow := mainbow.NewTab()

		fmt.Printf("Got %d images on watchlist page\n", len(imageIDs))
		if len(imageIDs) == 0 {
			fmt.Printf("No new submissions in watchlist, exiting")
			return
		}

		for _, imageID := range imageIDs {
			fmt.Printf("Going to image page %s.", imageID)
			rawurl := fmt.Sprintf("%s/view/%s", URLbase, imageID)
			imagePageURL, err := url.Parse(rawurl)
			if err != nil {
				fmt.Printf("Got error while parsing URL %s: %v\n", rawurl, err)
				continue
			}
			// check if it's in db and skip if it is
			isDownloaded, err := dbCheckIfDownloaded(dbpool, imagePageURL)
			if err != nil {
				fmt.Printf("Failed querying database, will download anyway: %s\n", err)
			}
			fmt.Printf(".")
			if isDownloaded {
				fmt.Printf(" skipped (already in database)\n")
				continue
			}
			err = openURL(imagebow, imagePageURL.String())
			if err != nil {
				fmt.Printf("Got error while getting %s: %v\n", imagePageURL, err)
				continue
			}
			fmt.Printf(".")

			artist := imagebow.Find("#submission_page div.submission-id-sub-container a strong").Text()
			if artist != "" {
				fmt.Printf(" by %s...", artist)
			}

			var imageURL *url.URL
			for _, link := range imagebow.Links() {
				if link.Text == "Download" {
					imageURL = link.URL
				}
			}

			if imageURL == nil {
				fmt.Printf("Page %s does not have image link -- skipping (page title is %s)\n", imagePageURL, imagebow.Title())
				continue
			}

			fmt.Printf(" queued\n")
			err = downloadImage(dbpool, strings.ToLower(artist), imagePageURL, imageURL)
			if err != nil {
				fmt.Printf("Failed to download image %s: %s\n", imageURL, err)
			}
			err = form.Check(imageID)
			if err != nil {
				fmt.Printf("Failed to checkmark image ID %s in the form\n", imageID)
			}
		}
	}
}

func downloadImage(dbpool *sqlitex.Pool, artist string, imagePageURL *url.URL, imageURL *url.URL) error {
	filename := path.Base(imageURL.Path)

	// if it's "1234567890." (sometimes it happens), then append artist name
	if m := brokenFilename.FindString(filename); len(m) != 0 && artist != "" {
		filename = filename + artist + ".unnamedimage.jpg"
	}

	fmt.Printf("Downloading image %s", filename)
	filepath := path.Join(opts.DownloadDirectory, filename)

	// get image's size
	contentLength, err := func() (int64, error) {
		resp, err := http.Head(imageURL.String())
		if err != nil {
			return 0, fmt.Errorf("Failed to HEAD on URL '%s': %w", imageURL, err)
		}
		contentLength := resp.ContentLength
		if resp.Body != nil {
			resp.Body.Close()
		}
		return contentLength, nil
	}()
	if err != nil {
		return fmt.Errorf("Failed to get content length of image at '%s': %w", imageURL, err)
	}
	fmt.Printf(".") // 1

	// check if file exists and filesize matches
	var stat os.FileInfo
	var lastModified time.Time
	if stat, err = os.Stat(filepath); err == nil {
		if int64(contentLength) == stat.Size() {
			// skip, file exists and size matches
			lastModified = setimagetime(filepath)
			fmt.Printf(" skipped (already exists and filesize matches)\n")
			// save to database
			err = dbSetImageURL(dbpool, imagePageURL, imageURL, lastModified, filename)
			if err != nil {
				return fmt.Errorf("Failed updating database: %w", err)
			}
			return nil // nothing else needs to be done
		}
	}
	fmt.Printf(".") // 2

	// fetch the image
	resp, err := http.Get(imageURL.String())
	if resp != nil { // even if err != nil, resp can be not nil as well
		defer resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("Failed to get URL '%s': %w", imageURL, err)
	}
	fmt.Printf(".") // 3

	// create temporary download file
	out, err := os.Create(filepath + ".download")
	if err != nil {
		return fmt.Errorf("Failed to create file '%s': %w", filepath, err)
	}
	defer out.Close()
	fmt.Printf(".") // 4

	// save the image
	// err = resp.BodyWriteTo(out)
	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("Failed to download URL '%s': %w", imageURL, err)
	}
	fmt.Printf(".") // 5

	if written != contentLength {
		return fmt.Errorf("Content length of %v != %v written, not marking as done\n", contentLength, written)
	}

	// get last-modified
	lastmod := resp.Header.Get("Last-Modified")
	if len(lastmod) != 0 {
		lastModified, err = time.Parse(time.RFC1123, lastmod)
		if err != nil {
			return fmt.Errorf("Failed to parse lastModified from %s, ignoring lastmodified: %w", string(lastmod), err)
		}
	}

	// rename temporary file to proper name
	err = os.Rename(filepath+".download", filepath)
	if err != nil {
		return fmt.Errorf("Failed to rename %s to %s: %w", filename+".download", filename, err)
	}
	fmt.Printf(".") // 6

	// set file's time
	setimagetime(filepath)
	fmt.Printf(".") // 7

	// save to database
	err = dbSetImageURL(dbpool, imagePageURL, imageURL, lastModified, filename)
	if err != nil {
		return fmt.Errorf("Failed updating database: %w", err)
	}
	fmt.Printf(" %v bytes written\n", contentLength)
	return nil
}

// ----------------
// helper functions
// ----------------

// check if it's in db and skip if it is
func dbCheckIfDownloaded(dbpool *sqlitex.Pool, URL *url.URL) (bool, error) {
	db := dbpool.Get(context.Background())
	if db == nil {
		return false, fmt.Errorf("Couldn't get db from dbpool")
	}
	defer dbpool.Put(db)

	dbkey := URL.Path
	var filename string
	fn := func(stmt *sqlite.Stmt) error {
		filename = stmt.ColumnText(0)
		return nil
	}
	err := sqlitex.Exec(db, "SELECT filename FROM image_urls WHERE page_url = ? LIMIT 1", fn, dbkey)
	if err != nil {
		return false, err
	} else if filename != "" {
		return true, nil
	}
	return false, nil
}

func dbMustExecute(db *sqlite.Conn, pragma string) {
	err := sqlitex.ExecTransient(db, pragma, nil)
	if err != nil {
		panic(fmt.Sprintf("Failed to execute statement %s: %s", pragma, err))
	}
}

func dbSetImageURL(dbpool *sqlitex.Pool, imagePageURL *url.URL, imageURL *url.URL, lastModified time.Time, filename string) error {
	db := dbpool.Get(context.Background())
	if db == nil {
		return fmt.Errorf("Couldn't get db from dbpool")
	}
	defer dbpool.Put(db)
	dbkey := imagePageURL.Path
	stmt, err := db.Prepare("INSERT OR REPLACE INTO image_urls (page_url, image_url, last_modified, filename) VALUES ($page_url, $image_url, $last_modified, $filename)")
	if err != nil {
		fmt.Printf("Couldn't prepare SQL query for setting image url: %s\n", err)
		return err
	}
	stmt.SetText("$page_url", dbkey)
	stmt.SetText("$image_url", imageURL.String())
	stmt.SetText("$last_modified", lastModified.String())
	stmt.SetText("$filename", filename)
	for {
		if hasRow, err := stmt.Step(); err != nil {
			fmt.Printf("Couldn't execute SQL query for setting image url: %s\n", err)
			return err
		} else if !hasRow {
			break
		}
	}
	return nil
}
func setimagetime(filepath string) time.Time {
	var t time.Time
	filename := path.Base(filepath)
	m := firstTenDigits.FindString(filename)
	if len(m) == 0 {
		return t
	}
	value, err := strconv.ParseInt(m, 10, 64)
	if err != nil {
		fmt.Printf("Couldn't parse %v into uint, skipping: %v\n", m, err)
		return t
	}
	t = time.Unix(value, 0)
	if t.Year() < 2000 {
		fmt.Printf("Skipping %v (%v) for %s because year was less than 2000", value, t, filename)
		return t
	}
	if time.Now().Before(t) {
		fmt.Printf("Skipping %v (%v) for %s because time is in the future", value, t, filename)
		return t
	}
	err = os.Chtimes(filepath, t, t)
	if err != nil {
		fmt.Printf("Couldn't change file %s time: %v\n", filepath, err)
		return t
	}
	return t
}

func updateDefaults(parser *flags.Parser) {
	// expand ~ into home directory
	expandDefaultDownloadDirectory(parser)
	// set config directory to platform standard
	setDefaultConfigDirectory(parser)
}

func expandDefaultDownloadDirectory(parser *flags.Parser) {
	option := parser.Command.FindOptionByLongName("download-directory")
	if option == nil {
		panic("SHOULD NOT HAPPEN: option is nil")
	}
	path := option.Default[0]
	newpath, err := homedir.Expand(path)
	if err != nil {
		panic(err)
	}
	option.Default[0] = newpath
	option.DefaultMask = path
}

func setDefaultConfigDirectory(parser *flags.Parser) {
	option := parser.Command.FindOptionByLongName("config-directory")
	if option == nil {
		panic("SHOULD NOT HAPPEN: option is nil")
	}
	configpath := configdir.LocalConfig("FA Downloader")
	option.Default = []string{configpath}

	// replace full path to home directory with ~
	home, err := homedir.Dir()
	if err != nil {
		panic(err)
	}
	if strings.HasPrefix(configpath, home) {
		option.DefaultMask = strings.Replace(configpath, home, "~", 1)
	}
}

func attr2map(attr []html.Attribute) map[string]string {
	attrMap := map[string]string{}
	for _, attribute := range attr {
		if attribute.Namespace != "" {
			continue
		}
		attrMap[strings.ToLower(attribute.Key)] = attribute.Val
	}
	return attrMap
}
