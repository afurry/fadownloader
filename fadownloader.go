package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/headzoo/surf"
	"github.com/jessevdk/go-flags"
	"github.com/juju/persistent-cookiejar"
	"github.com/kirsle/configdir"
	"github.com/mitchellh/go-homedir"
	"go.uber.org/ratelimit"
	"vbom.ml/util/sortorder"
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
	ConfigDir         string `short:"c" long:"config-directory" description:"Specify config directory" value-name:"dir"`
	DownloadDirectory string `short:"d" long:"download-directory" description:"Specify download directory" value-name:"dir" default:"~/Pictures/FADownloader"`
	Help              bool   `short:"h" long:"help" description:"Display this help message"`
}

var rl = ratelimit.New(3, ratelimit.WithoutSlack)
var bow = surf.NewBrowser()
var jar *cookiejar.Jar
var firstTenDigits = regexp.MustCompile(`^\d{10}`)

func main() {
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

	imagePages := map[string]string{}

	for _, artist := range args {
		fmt.Printf("Handling artist %s...\n", artist)
		startPage := fmt.Sprintf("https://www.furaffinity.net/gallery/%s/", artist)
		// go through every gallery page of this artist and collage image pages
		nextPageLink := startPage
		for nextPageLink != "" {
			galleryPage := nextPageLink
			nextPageLink = ""
			// haveImagePages := false
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
					// haveImagePages = true
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
		fmt.Printf("Handling image page %s...", URL.String())
		err = OpenURL(imagePage)
		if err != nil {
			fmt.Printf("Got error while getting %s: %v\n", imagePage, err)
			continue
		}

		image := ""

		fmt.Printf(".")
		for _, link := range bow.Links() {
			if link.Text == "Download" {
				image = link.URL.String()
			}
		}

		if image == "" {
			fmt.Printf("Page %s does not have image link -- skipping (page title is %s)\n", imagePage, bow.Title())
			continue
		}

		fmt.Printf(".")
		filename := path.Base(image)
		filepath := path.Join(opts.DownloadDirectory, filename)

		// create download directory if needed
		fmt.Printf(".")
		err = os.MkdirAll(opts.DownloadDirectory, 0777)
		if err != nil {
			fmt.Printf("Couldn't create download directory %s: %s\n", opts.DownloadDirectory, err)
			continue
		}

		// check if file exists
		if _, err := os.Stat(filepath); err == nil {
			setimagetime(filepath)
			fmt.Printf(" %s (already exists)\n", filename)
			continue
		}

		// create temporary download file
		fmt.Printf(".")
		out, err := os.Create(filepath + ".download")
		if err != nil {
			fmt.Printf("Failed to create file '%s': %s\n", filepath, err)
			continue
		}
		defer out.Close()

		// request the image
		fmt.Printf(".")
		resp, err := http.Get(image)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			fmt.Printf("Failed to get URL '%s': %s\n", image, err)
			continue
		}

		// save the image
		fmt.Printf(".")
		n, err := io.Copy(out, resp.Body)
		if err != nil {
			fmt.Printf("Failed to download URL '%s': %s\n", image, err)
			continue
		}

		// rename temporary file to proper name
		fmt.Printf(".")
		err = os.Rename(filepath+".download", filepath)
		if err != nil {
			fmt.Printf("Failed to rename %s to %s: %s\n", filename+".download", filename, err)
			continue
		}
		fmt.Printf(" %s (%v bytes)\n", filename, n)
	}
}

// ----------------
// helper functions
// ----------------
func setimagetime(filepath string) {
	filename := path.Base(filepath)
	m := firstTenDigits.FindString(filename)
	if len(m) == 0 {
		return
	}
	value, err := strconv.ParseInt(m, 10, 64)
	if err != nil {
		fmt.Printf("Couldn't parse %v into uint, skipping: %v\n", m, err)
		return
	}
	t := time.Unix(value, 0)
	if t.Year() < 2000 {
		fmt.Printf("Skipping %v (%v) for %s because year was less than 2000", value, t, filename)
		return
	}
	if time.Now().Before(t) {
		fmt.Printf("Skipping %v (%v) for %s because time is in the future", value, t, filename)
		return
	}
	err = os.Chtimes(filepath, t, t)
	if err != nil {
		fmt.Printf("Couldn't change file %s time: %v\n", filepath, err)
		return
	}
	return
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
