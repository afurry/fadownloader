package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"crawshaw.io/sqlite"
	"crawshaw.io/sqlite/sqliteutil"
	"github.com/headzoo/surf"
	"github.com/jessevdk/go-flags"
	"github.com/juju/persistent-cookiejar"
	"github.com/kirsle/configdir"
	"github.com/mitchellh/go-homedir"
	"github.com/valyala/fasthttp"
	"go.uber.org/ratelimit"
	"vbom.ml/util/sortorder"

	"net/http/pprof"
)

func isResponseOK(response *http.Response) bool {
	switch response.StatusCode {
	case 200:
		return true
	}
	fmt.Printf("Got response %+v\n", response)
	return false
}

func openURL(URL string) error {
	now := rl.Take()
	last = now

	err := bow.Open(URL)
	if err != nil {
		return err
	}

	if !isResponseOK(bow.State().Response) {
		return fmt.Errorf("Response is not ok")
	}
	return nil
}

var opts struct {
	NoFastScan        bool   `long:"no-fast-scan" description:"Disable fast scanning for artist's images"`
	NoGrabGallery     bool   `short:"g" long:"no-grab-gallery" description:"Don't grab artist's gallery"`
	GrabFavourites    bool   `short:"f" long:"grab-favourites" description:"Grab artist's favourites"`
	GrabScraps        bool   `short:"s" long:"grab-scraps" description:"Grab artist's scraps"`
	Help              bool   `short:"h" long:"help" description:"Display this help message"`
	ConfigDir         string `short:"c" long:"config-directory" description:"Specify config directory" value-name:"dir"`
	DownloadDirectory string `short:"d" long:"download-directory" description:"Specify download directory" value-name:"dir" default:"~/Pictures/FADownloader"`
}

var rl = ratelimit.New(3, ratelimit.WithoutSlack)
var bow = surf.NewBrowser()
var jar *cookiejar.Jar
var firstTenDigits = regexp.MustCompile(`^\d{10}`)
var brokenFilename = regexp.MustCompile(`^\d{10}\.$`)

func main() {
	err := setupPprof()
	if err == nil {
		defer pprofListener.Close()
	}
	parser := flags.NewParser(&opts, flags.PrintErrors|flags.PassDoubleDash|flags.PassAfterNonOption)

	// set custom usage line
	parser.Usage = "[options] artist1 [artist2 ...]"

	// update parser defaults with platform/user specific values
	updateDefaults(parser)

	// parse command line options
	artists, err := parser.Parse()
	if err != nil {
		panic(err)
	}

	// we do this to avoid having separate 'Help Options' section in help screen
	if opts.Help {
		parser.WriteHelp(os.Stdout)
		os.Exit(64)
	}

	fmt.Printf("Setting cookiejar\n")
	{
		cookiepath := path.Join(opts.ConfigDir, "cookies.json")
		fmt.Printf("cookie path - %s\n", cookiepath)
		jar, err = cookiejar.New(&cookiejar.Options{
			Filename: cookiepath,
		})
		if err != nil {
			panic(err)
		}
		bow.SetCookieJar(jar)
		bow.SetUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_3) AppleWebKit/536.28.10 (KHTML, like Gecko) Version/6.0.3 Safari/536.28.10")
		err = jar.Save()
		if err != nil {
			panic(err)
		}
	}
	defer jar.Save()

	// don't keep unlimited history, we never use the feature anyway
	bow.HistoryJar().SetMax(1)

	fmt.Printf("Opening database...")
	dbpool, err := sqlite.Open(path.Join(opts.ConfigDir, "downloaded.sqlite"), 0, 100)
	if err != nil {
		panic(err)
	}
	defer dbpool.Close()
	db := dbpool.Get(nil)
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

	imagePages := map[string]*string{}

	sort.Sort(sortorder.Natural(artists))
	for i, artist := range artists {
		fmt.Printf("Scanning artist %s (#%d of %d) for links...\n", artist, i, len(artists))
		pageTypes := []string{}
		if !opts.NoGrabGallery {
			pageTypes = append(pageTypes, "gallery")
		}
		if opts.GrabFavourites {
			pageTypes = append(pageTypes, "favorites")
		}
		if opts.GrabScraps {
			pageTypes = append(pageTypes, "scraps")
		}

		for _, pageType := range pageTypes {
			startPage := fmt.Sprintf("https://www.furaffinity.net/%s/%s/", pageType, artist)
			nextPageLink := startPage
			counter := 0
			for nextPageLink != "" {
				counter++
				galleryPage := nextPageLink
				nextPageLink = ""
				fmt.Printf("Going to %s's %s page #%d...", artist, pageType, counter)
				err := openURL(galleryPage)
				if err != nil {
					fmt.Printf("Got error while getting %s: %v\n", galleryPage, err)
					continue
				}

				fmt.Printf(".")
				newImagePages := map[*url.URL]*string{}
				for _, link := range bow.Links() {
					if strings.Contains(link.URL.Path, "/view/") {
						newImagePages[link.URL] = &artist
					}
					if link.Text == "Next  ❯❯" {
						url := link.URL.String()
						nextPageLink = url
					}
				}
				newImageCount := 0
				for k, v := range newImagePages {
					// if already downloaded, don't add it
					isDownloaded, _ := dbCheckIfDownloaded(db, k)
					if !isDownloaded {
						imagePages[k.String()] = v
						newImageCount++
					}
				}
				fmt.Printf(" Got %d valid and %d new images\n", len(newImagePages), newImageCount)
				if !opts.NoFastScan && newImageCount == 0 {
					break
				}
			}
		}
	}

	// sort
	keys := make([]string, 0, len(imagePages))
	for key := range imagePages {
		keys = append(keys, key)
	}

	sort.Sort(sortorder.Natural(keys))

	fmt.Printf("Will get total %d pictures\n", len(keys))

	var wg sync.WaitGroup
	for counter, imagePage := range keys {
		length := len(keys)
		URL, err := url.Parse(imagePage)
		if err != nil {
			fmt.Printf("Got error while parsing URL %s: %v\n", imagePage, err)
			continue
		}
		artist := imagePages[imagePage]
		fmt.Printf("[#%6d of %6d] Queuing %s\n", counter, length, URL.Path)
		// check if it's in db and skip if it is
		isDownloaded, err := dbCheckIfDownloaded(db, URL)
		if err != nil {
			fmt.Printf("[#%6d of %6d] Failed querying database, will download anyway: %s\n", counter, length, err)
		}
		if isDownloaded {
			fmt.Printf("[#%6d of %6d] Skipped (already in database)\n", counter, length)
			continue
		}
		err = openURL(imagePage)
		if err != nil {
			fmt.Printf("[#%6d of %6d] Got error while getting %s: %v\n", counter, length, imagePage, err)
			continue
		}

		var image *url.URL

		for _, link := range bow.Links() {
			if link.Text == "Download" {
				image = link.URL
			}
		}

		if image == nil {
			fmt.Printf("[#%6d of %6d] Page %s does not have image link -- skipping (page title is %s)\n", counter, length, imagePage, bow.Title())
			continue
		}

		wg.Add(1)
		go func(image url.URL, artist *string, dbpool *sqlite.Pool, URL url.URL, counter int, length int, wg *sync.WaitGroup) {
			defer wg.Done()
			filename := path.Base(image.Path)

			// if it's "1234567890." (sometimes it happens), then append artist name
			if m := brokenFilename.FindString(filename); len(m) != 0 {
				filename = filename + *artist + ".unnamedimage.jpg"
			}

			filepath := path.Join(opts.DownloadDirectory, filename)

			// create download directory if needed
			err := os.MkdirAll(opts.DownloadDirectory, 0700)
			if err != nil {
				fmt.Printf("[#%6d of %6d] Couldn't create download directory %s: %s\n", counter, length, opts.DownloadDirectory, err)
				return
			}

			// smaller scope so that we can close the file right after we're done with it
			var lastModified time.Time
			var contentLength int64
			// get image's size
			{
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(resp)
				req.SetRequestURI(image.String())
				req.Header.SetMethod("HEAD")
				err = fasthttp.Do(req, resp)
				if err != nil {
					fmt.Printf("[#%6d of %6d] Failed to HEAD on URL '%s': %s\n", counter, length, image.String(), err)
					return
				}
				contentLength = int64(resp.Header.ContentLength())
			}

			// check if file exists and filesize matches
			var stat os.FileInfo
			if stat, err = os.Stat(filepath); err == nil {
				if int64(contentLength) == stat.Size() {
					// skip, file exists and size matches
					lastModified = setimagetime(filepath)
					fmt.Printf("[#%6d of %6d] Skipped %s (already exists and filesize matches)\n", counter, length, filename)
					// save to database
					err = dbSetImageURL(dbpool, URL, image, lastModified, filename)
					if err != nil {
						fmt.Printf("[#%6d of %6d] Failed updating database: %s\n", counter, length, err)
						return
					}
					return
				}
			}

			// fetch the image
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI(image.String())
			err = fasthttp.Do(req, resp)
			if err != nil {
				fmt.Printf("[#%6d of %6d] Failed to get URL '%s': %s\n", counter, length, image.String(), err)
				return
			}

			// create temporary download file
			out, err := os.Create(filepath + ".download")
			if err != nil {
				fmt.Printf("[#%6d of %6d] Failed to create file '%s': %s\n", counter, length, filepath, err)
				return
			}
			defer out.Close()

			// save the image
			err = resp.BodyWriteTo(out)
			if err != nil {
				fmt.Printf("[#%6d of %6d] Failed to download URL '%s': %s\n", counter, length, image.String(), err)
				return
			}

			// get last-modified
			lastmod := resp.Header.Peek("Last-Modified")
			if len(lastmod) != 0 {
				lastModified, err = fasthttp.ParseHTTPDate(lastmod)
				if err != nil {
					fmt.Printf("[#%6d of %6d] Failed to parse lastModified from %s, ignoring lastmodified: %s\n", counter, length, string(lastmod), err)
					return
				}
			}

			// rename temporary file to proper name
			err = os.Rename(filepath+".download", filepath)
			if err != nil {
				fmt.Printf("[#%6d of %6d] Failed to rename %s to %s: %s\n", counter, length, filename+".download", filename, err)
				return
			}

			// set file's time
			setimagetime(filepath)

			// save to database
			err = dbSetImageURL(dbpool, URL, image, lastModified, filename)
			if err != nil {
				fmt.Printf("[#%6d of %6d] Failed updating database: %s\n", counter, length, err)
				return
			}
			fmt.Printf("[#%6d of %6d] Saved %s (%v bytes)\n", counter, length, filename, contentLength)
		}(*image, artist, dbpool, *URL, counter, length, &wg)
	}
	wg.Wait()
}

// ----------------
// helper functions
// ----------------

// check if it's in db and skip if it is
func dbCheckIfDownloaded(db *sqlite.Conn, URL *url.URL) (bool, error) {
	dbkey := URL.Path
	var filename string
	fn := func(stmt *sqlite.Stmt) error {
		filename = stmt.ColumnText(0)
		return nil
	}
	err := sqliteutil.Exec(db, "SELECT filename FROM image_urls WHERE page_url = ? LIMIT 1", fn, dbkey)
	if err != nil {
		return false, err
	} else if filename != "" {
		return true, nil
	}
	return false, nil
}

func dbMustExecute(db *sqlite.Conn, pragma string) {
	err := sqliteutil.ExecTransient(db, pragma, nil)
	if err != nil {
		panic(fmt.Sprintf("Failed to execute statement %s: %s", pragma, err))
	}
}

func dbSetImageURL(dbpool *sqlite.Pool, URL url.URL, image url.URL, lastModified time.Time, filename string) error {
	db := dbpool.Get(nil)
	if db == nil {
		return fmt.Errorf("Couldn't get db from dbpool")
	}
	defer dbpool.Put(db)
	dbkey := URL.Path
	stmt, err := db.Prepare("INSERT OR REPLACE INTO image_urls (page_url, image_url, last_modified, filename) VALUES ($page_url, $image_url, $last_modified, $filename)")
	if err != nil {
		fmt.Printf("Couldn't prepare SQL query for setting image url: %s\n", err)
		return err
	}
	stmt.SetText("$page_url", dbkey)
	stmt.SetText("$image_url", image.String())
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

var last = time.Now()
var pprofListener net.Listener

func setupPprof() error {
	var err error
	pprofListener, err = net.Listen("tcp", "localhost:6053")
	if err != nil {
		fmt.Printf("Failed to start pprof handler: %s", err)
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	go func() {
		http.Serve(pprofListener, mux)
	}()
	return nil
}
