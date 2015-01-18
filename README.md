#Furaffinity Downloader

This was made for personal use, but decided to upload just in case someone else finds it useful.

Before you can use it, install some gems:
```ruby
gem install mechanize naturalsort sqlite3 addressable
```

Usage:
```bash
./fadownloader.rb tojo-the-thief
```

Downloads all pictures from Tojo-The-Thief's gallery.

Default download path is `$HOME/Pictures/FADownloader`, but you can override that via `--download-directory <dir>` parameter.

For all parameters, run `./fadownloader.rb --help`
