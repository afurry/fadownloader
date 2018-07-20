package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"crawshaw.io/sqlite"
	"crawshaw.io/sqlite/sqliteutil"
	"github.com/headzoo/surf"
	"github.com/jessevdk/go-flags"
	"github.com/juju/persistent-cookiejar"
	"github.com/kirsle/configdir"
	"github.com/mitchellh/go-homedir"
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

func OpenURL(URL string) error {
	now := rl.Take()
	fmt.Printf("%.2fms", now.Sub(last).Seconds()*1000.0)
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
	NoGrabGallery     bool   `short:"g" long:"no-grab-gallery" description:"Don't grab artist's gallery"`
	GrabFavourites    bool   `short:"f" long:"grab-favourites" description:"Grab artist's favourites"`
	GrabScraps        bool   `short:"s" long:"grab-scraps" description:"Grab artist's scraps"`
	ConfigDir         string `short:"c" long:"config-directory" description:"Specify config directory" value-name:"dir"`
	DownloadDirectory string `short:"d" long:"download-directory" description:"Specify download directory" value-name:"dir" default:"~/Pictures/FADownloader"`
	Help              bool   `short:"h" long:"help" description:"Display this help message"`
}

var rl = ratelimit.New(3, ratelimit.WithoutSlack)
var bow = surf.NewBrowser()
var jar *cookiejar.Jar
var firstTenDigits = regexp.MustCompile(`^\d{10}`)
var brokenFilename = regexp.MustCompile(`^\d{10}\.$`)

func main() {
	setupPprof()
	defer pprofListener.Close()
	parser := flags.NewParser(&opts, flags.PrintErrors|flags.PassDoubleDash|flags.PassAfterNonOption)

	// set custom usage line
	parser.Usage = "[options] artist1 [artist2 ...]"

	// update parser defaults with platform/user specific values
	err := updateDefaults(parser)
	if err != nil {
		panic(err)
	}

	// parse command line options
	args, err := parser.Parse()
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
		var err error
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
	}
	jar.Save()
	defer jar.Save()

	// don't keep unlimited history, we never use the feature anyway
	bow.HistoryJar().SetMax(1)

	fmt.Printf("Opening database...")
	db, err := sqlite.OpenConn(path.Join(opts.ConfigDir, "downloaded.sqlite"), 0)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	DBMustExecute(db, "CREATE TABLE IF NOT EXISTS image_urls (page_url TEXT PRIMARY KEY UNIQUE, image_url TEXT, last_modified TEXT, filename TEXT)")
	DBMustExecute(db, "CREATE INDEX IF NOT EXISTS page_urls ON image_urls(page_url)")
	DBMustExecute(db, "PRAGMA cache_size = 1000000")
	DBMustExecute(db, "PRAGMA temp_store = MEMORY")
	DBMustExecute(db, "PRAGMA synchronous = OFF")
	DBMustExecute(db, "PRAGMA journal_mode = MEMORY")
	DBMustExecute(db, "PRAGMA busy_timeout = 5000")
	fmt.Printf("\n")

	imagePages := map[string]string{}

	for _, artist := range args {
		fmt.Printf("Handling artist %s...\n", artist)
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
			for nextPageLink != "" {
				galleryPage := nextPageLink
				nextPageLink = ""
				fmt.Printf("Handling page %s...", galleryPage)
				err := OpenURL(galleryPage)
				if err != nil {
					fmt.Printf("Got error while getting %s: %v\n", galleryPage, err)
					continue
				}

				fmt.Printf(".")
				newImagePages := map[string]string{}
				for _, link := range bow.Links() {
					if strings.Contains(link.URL.Path, "/view/") {
						newImagePages[link.URL.String()] = artist
					}
					if link.Text == "Next  ❯❯" {
						url := link.URL.String()
						nextPageLink = url
					}
				}
				fmt.Printf(" Got %d images\n", len(newImagePages))
				for k, v := range newImagePages {
					imagePages[k] = v
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

	for _, imagePage := range keys {
		URL, err := url.Parse(imagePage)
		if err != nil {
			fmt.Printf("Got error while parsing URL %s: %v\n", imagePage, err)
			continue
		}
		artist := imagePages[imagePage]
		fmt.Printf("Handling image page %s...", URL.String())
		// check if it's in db and skip if it is
		{
			dbkey := URL.Path
			var filename string
			fn := func(stmt *sqlite.Stmt) error {
				filename = stmt.ColumnText(0)
				return nil
			}
			err := sqliteutil.Exec(db, "SELECT filename FROM image_urls WHERE page_url = ? LIMIT 1", fn, dbkey)
			if err != nil {
				fmt.Printf("Failed querying database, will download anyway: %s\n", err)
			} else if filename != "" {
				fmt.Printf(" %s (already in database)\n", filename)
				continue
			}
		}
		err = OpenURL(imagePage)
		if err != nil {
			fmt.Printf("Got error while getting %s: %v\n", imagePage, err)
			continue
		}

		var image *url.URL

		fmt.Printf(".")
		for _, link := range bow.Links() {
			if link.Text == "Download" {
				image = link.URL
			}
		}

		if image == nil {
			fmt.Printf("Page %s does not have image link -- skipping (page title is %s)\n", imagePage, bow.Title())
			continue
		}

		fmt.Printf(".")
		filename := path.Base(image.Path)

		// if it's "1234567890." (sometimes it happens), then append artist name
		if m := brokenFilename.FindString(filename); len(m) != 0 {
			filename = filename + artist + ".unnamedimage.jpg"
		}

		filepath := path.Join(opts.DownloadDirectory, filename)

		// create download directory if needed
		fmt.Printf(".")
		err = os.MkdirAll(opts.DownloadDirectory, 0777)
		if err != nil {
			fmt.Printf("Couldn't create download directory %s: %s\n", opts.DownloadDirectory, err)
			continue
		}

		// smaller scope so that we can close the file right after we're done with it
		var lastModified time.Time
		var bytesWritten int64
		{
			// request the image
			fmt.Printf(".")
			resp, err := http.Get(image.String())
			if resp != nil {
				defer resp.Body.Close()
			}
			if err != nil {
				fmt.Printf("Failed to get URL '%s': %s\n", image, err)
				continue
			}

			// check if file exists and filesize matches
			fmt.Printf(".")
			if stat, err := os.Stat(filepath); err == nil {
				if imagesize, err := parseContentLength(resp.Header.Get("Content-Length")); err != nil {
					fmt.Printf("failed parsing content-length, redownloading file anyway: %s\n", err)
				} else {
					if imagesize == stat.Size() {
						// skip, file exists and size matches
						lastModified := setimagetime(filepath)
						fmt.Printf(" %s (already exists)\n", filename)
						// save to database
						err = DBSetImageURL(db, URL, image, lastModified, filename)
						if err != nil {
							fmt.Printf("Failed updating database: %s\n", err)
							continue
						}
						continue
					}
				}
			}

			// create temporary download file
			fmt.Printf(".")
			out, err := os.Create(filepath + ".download")
			if err != nil {
				fmt.Printf("Failed to create file '%s': %s\n", filepath, err)
				continue
			}
			defer out.Close()

			// save the image
			fmt.Printf(".")
			bytesWritten, err = io.Copy(out, resp.Body)
			if err != nil {
				fmt.Printf("Failed to download URL '%s': %s\n", image, err)
				continue
			}

			// get last-modified
			fmt.Printf(".")
			lastmod := resp.Header.Get("Last-Modified")
			if lastmod != "" {
				lastModified, err = http.ParseTime(lastmod)
			}
		}

		// rename temporary file to proper name
		fmt.Printf(".")
		err = os.Rename(filepath+".download", filepath)
		if err != nil {
			fmt.Printf("Failed to rename %s to %s: %s\n", filename+".download", filename, err)
			continue
		}

		// set file's time
		fmt.Printf(".")
		setimagetime(filepath)

		// save to database
		fmt.Printf(".")
		err = DBSetImageURL(db, URL, image, lastModified, filename)
		if err != nil {
			fmt.Printf("Failed updating database: %s\n", err)
			continue
		}
		fmt.Printf(" %s (%v bytes)\n", filename, bytesWritten)
	}
}

// ----------------
// helper functions
// ----------------

// parseContentLength trims whitespace from s and returns -1 if no value
// is set, or the value if it's >= 0.
func parseContentLength(cl string) (int64, error) {
	cl = strings.TrimSpace(cl)
	if cl == "" {
		return -1, nil
	}
	n, err := strconv.ParseInt(cl, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("Bad Content-Length: \"%s\"", cl)
	}
	return n, nil

}

func DBMustExecute(db *sqlite.Conn, pragma string) {
	err := sqliteutil.ExecTransient(db, pragma, nil)
	if err != nil {
		panic(fmt.Sprintf("Failed to execute statement %s: %s", pragma, err))
	}
}

func DBSetImageURL(db *sqlite.Conn, URL *url.URL, image *url.URL, lastModified time.Time, filename string) error {
	dbkey := URL.Path
	err := sqliteutil.Exec(db, "INSERT OR REPLACE INTO image_urls (page_url, image_url, last_modified, filename) VALUES (?, ?, ?, ?)", nil, dbkey, image, lastModified, filename)
	return err
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

func updateDefaults(parser *flags.Parser) error {
	// expand ~ into home directory
	err := expandDefaultDownloadDirectory(parser)
	if err != nil {
		return err
	}
	// set config directory to platform standard
	err = setDefaultConfigDirectory(parser)
	if err != nil {
		return err
	}
	return nil
}

func expandDefaultDownloadDirectory(parser *flags.Parser) error {
	option := parser.Command.FindOptionByLongName("download-directory")
	if option == nil {
		return fmt.Errorf("SHOULD NOT HAPPEN: option is nil")
	}
	path := option.Default[0]
	newpath, err := homedir.Expand(path)
	if err != nil {
		return err
	}
	option.Default[0] = newpath
	option.DefaultMask = path
	return nil
}

func setDefaultConfigDirectory(parser *flags.Parser) error {
	option := parser.Command.FindOptionByLongName("config-directory")
	if option == nil {
		return fmt.Errorf("SHOULD NOT HAPPEN: option is nil")
	}
	configpath := configdir.LocalConfig("FA Downloader")
	option.Default = []string{configpath}

	// replace full path to home directory with ~
	home, err := homedir.Dir()
	if err != nil {
		return err
	}
	if strings.HasPrefix(configpath, home) {
		option.DefaultMask = strings.Replace(configpath, home, "~", 1)
	}
	return nil
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
