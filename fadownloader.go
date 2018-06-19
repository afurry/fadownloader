package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/beefsack/go-rate"
	"github.com/headzoo/surf"
	"github.com/headzoo/surf/browser"
	"github.com/juju/persistent-cookiejar"
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

func handleLinks(URL string, handler func(link *browser.Link)) error {
	ok, remaining := rl.Try()
	if !ok {
		fmt.Printf("Ratelimit exceeded, sleeping %v...\n", remaining)
		time.Sleep(remaining)
	}

	fmt.Printf("Opening %s...\n", URL)
	err := bow.Open(URL)
	if err != nil {
		panic(err)
	}

	if !isResponseOK(bow.State().Response) {
		fmt.Printf("Response is not ok -- stopping\n")
		return fmt.Errorf("Response is not ok")
	}

	for _, link := range bow.Links() {
		handler(link)
	}

	return nil
}

var rl = rate.New(2, time.Second)
var bow = surf.NewBrowser()
var jar *cookiejar.Jar

func main() {
	imagePages := map[string]bool{}
	images := map[string]bool{}

	{
		var err error
		jar, err = cookiejar.New(&cookiejar.Options{
			Filename: "cookies.json",
		})
		if err != nil {
			panic(err)
		}
		fmt.Printf("jar type = %T\n", jar)
		bow.SetCookieJar(jar)
		bow.SetUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_3) AppleWebKit/536.28.10 (KHTML, like Gecko) Version/6.0.3 Safari/536.28.10")
	}
	jar.Save()
	defer jar.Save()

	startPage := "https://www.furaffinity.net/gallery/wolfy-nail/"

	// go through every gallery page of this artist and collage image pages
	nextPageLink := &startPage
	for {
		galleryPage := *nextPageLink
		nextPageLink = nil

		// grab image links from gallery page
		haveImagePages := false
		err := handleLinks(galleryPage, func(link *browser.Link) {
			if strings.Contains(link.URL.Path, "/view/") {
				imagePages[link.URL.String()] = true
				haveImagePages = true
			}
			if link.Text == "Next  ❯❯" {
				url := link.URL.String()
				nextPageLink = &url
			}
		})

		if err != nil {
			panic(err)
		}

		if !haveImagePages {
			fmt.Printf("Page %s does not have image pages -- skipping (page title is %s)\n", galleryPage, bow.Title())
		}

		if nextPageLink == nil {
			break
		}
	}

	{
		// sort
		keys := make([]string, 0, len(imagePages))
		for key := range imagePages {
			keys = append(keys, key)
		}

		sort.Sort(sortorder.Natural(keys))

		for _, key := range keys {
			haveLinks := false
			err := handleLinks(key, func(link *browser.Link) {
				if link.Text == "Download" {
					images[link.URL.String()] = true
					haveLinks = true
				}
			})

			if err != nil {
				panic(err)
			}

			if !haveLinks {
				fmt.Printf("Page %s does not have image link -- skipping (page title is %s)\n", key, bow.Title())
			}
		}
	}

	{
		// sort
		keys := make([]string, 0, len(images))
		for key := range images {
			keys = append(keys, key)
		}

		sort.Sort(sortorder.Natural(keys))

		for _, key := range keys {
			fmt.Printf("Downloading %s... ", key)
			filename := path.Base(key)
			out, err := os.Create(filename + ".download")
			if err != nil {
				fmt.Printf("Failed to create file '%s': %s\n", filename, err)
				continue
			}
			defer out.Close()

			resp, err := http.Get(key)
			if resp != nil {
				defer resp.Body.Close()
			}
			if err != nil {
				fmt.Printf("Failed to get URL '%s': %s\n", key, err)
				continue
			}

			n, err := io.Copy(out, resp.Body)
			if err != nil {
				fmt.Printf("Failed to download URL '%s': %s\n", key, err)
				continue
			}
			err = os.Rename(filename+".download", filename)
			if err != nil {
				fmt.Printf("Failed to rename %s to %s: %s\n", filename+".download", filename, err)
				continue
			}
			fmt.Printf("done (%v bytes)\n", n)
		}
	}
}
