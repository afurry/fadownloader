#!/usr/bin/env ruby

# gem install mechanize naturalsort sqlite3 addressable

# built-in
require 'optparse'
require 'logger'

# ruby gems
require 'rubygems'
require 'mechanize'
require 'rbconfig'
require 'natural_sort_kernel'
require 'yaml'

# our own
$: << File.dirname(__FILE__)
require 'fadownloader_common'

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

  opts.on('-d', '--download-directory dir', String, "Specify download directory (default: #{appconfig.download_directory})") do |v| 
    appconfig.download_directory = v if v
  end

  opts.on_tail('-h', '--help', 'Display this screen') do puts opts; exit end
end

optparse.parse!

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
appconfig.loadconfig

## load cookies
agent.cookie_jar.load(appconfig[:cookies_filepath], :cookiestxt) if File.exists?(appconfig[:cookies_filepath])

## Login
logs 'Going to front page to check login'
page = agent.get(appconfig[:url_base])
page = check_and_login(agent, page)

# Go to watchlist page
logs 'Loading watchlist submissions'
url = appconfig[:url_base] + "/" + appconfig[:url_watchlist_submissions]
page = agent.get(url)

didsleep = false

while true do
  form = page.form_with(:name => 'messages-form')
  if form == nil
    logs "No images on page, exiting"
    exit
  end
  didsleep = false
  checkboxes = form.checkboxes_with(:name => /submissions/)
  logs 'Got ' + checkboxes.length.to_s + ' images on watchlist page'
  if checkboxes.length == 0
    logs "No new submissions in watchlist, exiting"
    exit
  end

  # extract images to download from these checkboxes
  pictures = Hash.new
  checkboxes.each do |box|
    image_url = '/view/' + box.value + '/'
    pictures[image_url] = box.value
  end

  logs "Nothing new to download" if pictures.length == 0

  FileUtils.mkpath appconfig.download_directory

  ## download gathered links
  counter = 0
  pictures.keys.natural_sort.each do |key|
    counter += 1
    # image without an href -- deleted image, don't download, already marked for removal by FA
    link = page.link_with(:href => key)
    if link == nil
      logs "Image " + key + " was deleted by author, marking as viewed"
      form.checkbox_with(:value => pictures[key]).check
      next
    end

    log_print "Getting image #{key} (#{counter} of #{pictures.length})"

    ## get image
    filename = downloadfrompage(key, agent, db)
    
    ## if success, mark image for removal from watchlist
    form.checkbox_with(:value => pictures[key]).check if filename != nil
  end

  numchecked = form.checkboxes_with(:name => /submissions/, :checked => true).length
  logs "Marking #{numchecked} images as viewed"
  button = form.button_with(:value => "Remove checked")
  page = form.click_button(button)

end
