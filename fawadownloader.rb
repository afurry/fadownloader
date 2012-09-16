#!/usr/bin/env ruby

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
  
#  p page

  form = page.form_with(:name => 'messages-form')
  if form == nil
    sleeptime = 60
    logs "No images on page, checking page every #{sleeptime} seconds" if didsleep == false
    exit
    sleep sleeptime
    page = agent.get(url)
    didsleep = true
    next
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

    

    # if in database, we've downloaded it already, mark image for removal
#    box.check if dbvalue != nil
#    next if dbvalue != nil


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
    logs "Image " + key + " was deleted by author, skipping it" if link == nil
    next if link == nil

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
