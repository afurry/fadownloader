#!/usr/bin/env ruby

$: << File.dirname(__FILE__)
require 'fadownloader_common'

## supports downloading multiple artists
## TODO: don't download images if their metadata matches
## TODO: add EXIF tags to jpeg images -- original url, keywords, image name and image description
## TODO: get username and password from osx keychain
## TODO: generate empty config file if it doesn't exist
## TODO: use a HEAD request and compare metadata
appconfig = AppConfig.instance



FileUtils.mkpath appconfig[:settings_directory]

#####################
## parse command line
#####################
optparse = OptionParser.new do |opts|
  opts.banner = "Usage: fadownloader.rb [options] artist1 artist2 ..."

  appconfig.verbose = true
  opts.on('-q', '--quiet', 'Output less info') do appconfig.verbose = false end

  appconfig.gallery = true
  opts.on('-g', '--[no-]gallery', "Download artist's gallery (default: #{appconfig.gallery})") do |v| appconfig.gallery = v end

  appconfig.favourites = false
  opts.on('-f', '--[no-]favourites', "Also download artist's favourites (default: #{appconfig.favourites})") do |v| appconfig.favourites = v end

  appconfig.scraps = false
  opts.on('-s', '--[no-]scraps', "Also download artist\'s scraps (default: #{appconfig.scraps})") do |v| appconfig.scraps = v end

  appconfig.fastscan = true
  opts.on('-f', '--[no-]fast-scan', "Fast artist scanning (default: #{appconfig.fastscan})") do |v| appconfig.fastscan = v end

  opts.on('-d', '--download-directory dir', String, "Specify download directory (default: #{appconfig.download_directory})") do |v| 
    appconfig.download_directory = v if v
  end

  opts.on_tail('-h', '--help', 'Display this screen') do puts opts; exit end
end

optparse.parse!
if ARGV.empty?
  puts optparse
  exit
end

#############
## initialize
#############
logs "Being verbose"
#agent = Mechanize.new { |a| a.log = Logger.new("mech.log") }
agent = Mechanize.new
agent.max_history = 0
agent.user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_3) AppleWebKit/536.28.10 (KHTML, like Gecko) Version/6.0.3 Safari/536.28.10"

## initialise database
db = AppDatabase.new(appconfig[:database_filepath])

## load config
begin
  appconfig.loadconfig
rescue
  $stderr.puts "Couldn't load configuration -- #{$!.inspect}"
  $stderr.puts ""
  $stderr.puts "Please create a file '#{appconfig[:config_filepath]}' with contents like this:"
  $stderr.puts "username: <your username>"
  $stderr.puts "password: <your password>"
  $stderr.puts ""
  $stderr.puts "And run this program again"
  exit 1
end

## load cookies
agent.cookie_jar.load(appconfig[:cookies_filepath], :cookiestxt) if File.exists?(appconfig[:cookies_filepath])

## Login
logs 'Going to front page to check login'
page = agent.get(appconfig[:url_base])
page = check_and_login(agent, page)

pictures = Hash.new

## gather links
agent.history_added = Proc.new { sleep 0.1 }
ARGV.natural_sort.each do |artistname|
  logs "Scanning artist #{artistname} for links..."
  pictures.merge!(gather_links_from_artist(db, agent, page, artistname, appconfig[:url_gallery])) if appconfig.gallery
  pictures.merge!(gather_links_from_artist(db, agent, page, artistname, appconfig[:url_favourites])) if appconfig.favourites
  pictures.merge!(gather_links_from_artist(db, agent, page, artistname, appconfig[:url_scraps])) if appconfig.scraps
end
agent.history_added = nil

puts "Nothing to download" if pictures.length == 0
exit if pictures.length == 0
logs "Will get total #{pictures.length} pictures"

FileUtils.mkpath appconfig.download_directory

## download gathered links
counter = 0
pictures.keys.natural_sort.each do |key|
  counter += 1

  ## get from database
  image_url, last_modified = db[key]

  next if image_url != nil

  log_print "Getting image #{key} (#{counter} of #{pictures.length})"

  filename = downloadfrompage(key, agent, db)
end
