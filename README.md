#Furaffinity Downloader

This was made for personal use, but decided to upload just in case someone else finds it useful and wants to have a personal copy of fur affinity art.

Before you can use it, install some gems:
```ruby
gem install mechanize naturalsort sqlite3 addressable
```

Usage:
```bash
./fadownloader.rb tojo-the-thief
```

Downloads all pictures from Tojo-The-Thief's gallery and remembers which pictures you've successfully downloaded.

On next launch, it won't download them again.

***

Default download path is `$HOME/Pictures/FADownloader`, but you can override that via `--download-directory <dir>` parameter.

Database where list of downloaded pictures is stored at:
 * OSX -- `~/Library/Application Support/FA Downloader`
 * Unix -- `~/.fadownloader`
 * Windows -- `~/.FA Downloader`

For all parameters, run `./fadownloader.rb --help`
