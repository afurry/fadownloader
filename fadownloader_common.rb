#!/usr/bin/env ruby

begin
require 'rubygems'
require 'bundler/setup'
rescue LoadError => e
    print "Missing gems: #{e.inspect}\n\n"
    print "Do this: gem install pkg-config bundler && bundler\n\n"
    exit
end

# built-in
require 'optparse'
require 'logger'

# ruby gems
begin
require 'rubygems'
require 'mechanize'
require 'sqlite3'
require 'rbconfig'
require 'natural_sort_kernel'
require 'yaml'
require 'singleton'
require 'addressable/uri'
require 'xdg'
require 'pp'
rescue LoadError => e
    print "Missing gems: #{e.inspect}\n\n"
    print "Do this: gem install mechanize naturalsort sqlite3 addressable xdg\n\n"
    exit
end

class AppConfig
  include Singleton

  def initialize
    @data = {}
    @data[:url_base] = 'https://www.furaffinity.net/'
    @data[:url_login] = 'login'
    @data[:url_gallery] = 'gallery'
    @data[:url_favourites] = 'favorites'
    @data[:url_scraps] = 'scraps'
    @data[:url_watchlist_submissions] = 'msg/submissions/'

    @data[:program_identity_short] = "fadownloader" # used for settings dir on linux
    @data[:program_identity_lean] = "FA Downloader" # used for settings dir on Mac/Windows
    @data[:program_identity_lean_nospace] = "FADownloader" # used for settings dir on Mac/Windows
    @data[:program_identity_full] = "FurAffinity Downloader" # full program name

    @data[:database_defaultname] = 'downloaded.sqlite'
    @data[:configfile] = 'config.yaml'

    #####################
    ## Settings/Options
    ## Different paths for linux/macosx/windows
    #####################
    case RbConfig::CONFIG['host_os']
    when /darwin/i
      # MacOSX
      @data[:settings_directory] = File.expand_path("~/Library/Application Support/#{@data[:program_identity_lean]}")
    else
      # Generic unix
      @data[:settings_directory] = File.expand_path("#{XDG['CONFIG_HOME']}/#{@data[:program_identity_short]}")
    end

    @data[:cookies_filepath] = File.expand_path("#{@data[:settings_directory]}/cookies.txt")
    @data[:database_filepath] = File.expand_path("#{@data[:settings_directory]}/#{@data[:database_defaultname]}")
    self.download_directory = File.expand_path("~/Pictures/#{@data[:program_identity_lean_nospace]}")
    @data[:config_filepath] = File.expand_path("#{@data[:settings_directory]}/#{@data[:configfile]}")

  end


  def [](key)
    @data[key.to_sym]
  end
  
  attr_accessor :verbose
  attr_accessor :gallery
  attr_accessor :favourites
  attr_accessor :scraps
  attr_accessor :fastscan
  attr_accessor :download_directory
  attr_accessor :username
  attr_accessor :password

  def loadconfig
    loaded = Hash[YAML::load_file(@data[:config_filepath]).map { |k, v| [k.to_sym, v] }]
    self.username = loaded[:username]
    self.password = loaded[:password]
  end
  
  def username=(value)
    @username = value
    @data[:cookies_filepath] = File.expand_path("#{@data[:settings_directory]}/cookies.#{value}.txt")
  end
end


###################
## helper functions
###################
def log_print(string)
  appconfig = AppConfig.instance
  return if !appconfig.verbose
  print string
  $stdout.flush
end

def logs(string)
  appconfig = AppConfig.instance
  return if !appconfig.verbose
  puts string
end

##
## login/logout
##
def do_login(agent, page)
  appconfig = AppConfig.instance
  ## get login page
  logs 'Going to login page'
  page = page.link_with(:text => 'Log in', :href => /\/login\//).click
  puts 'page is nil' if !page
  return nil if !page

  agent.cookie_jar.save_as(appconfig[:cookies_filepath], :cookiestxt)

  ## fill a login form
  login_form = page.form_with(:action => /\/login\//)
  login_form.field_with(:name => "name").value = appconfig.username
  login_form.field_with(:name => "pass").value = appconfig.password

  ## submit a filled form
  logs 'Logging in'
  page = agent.submit(login_form, login_form.buttons.first)
  return page
end

def check_and_login(agent, page)
  appconfig = AppConfig.instance
  ## check if we need to log in
  link = page.link_with(:href => /\/login/)
  agent.cookie_jar.save_as(appconfig[:cookies_filepath], :cookiestxt)
  if link.text == "Log in"
    logs "Not logged in, need to log in"
    page = do_login(agent, page)
    agent.cookie_jar.save_as(appconfig[:cookies_filepath], :cookiestxt)
  end

  ## verify that we're actually logged in
  raise "Could not log in!" if page.link_with(:href => /^\/user/).text == "Guest"
  return page
end

##
## gather links from artist's pages
##
def gather_links_from_artist(db, agent, page, artistname, url_listing)
  appconfig = AppConfig.instance
  pagenum = 1
  validlinks = Hash.new
  page = check_and_login(agent, page)
  escaped_artist_name = CGI::escape(artistname)
  listing_url = "#{appconfig[:url_base]}/#{url_listing}/#{escaped_artist_name}/"
  log_print "Going to #{artistname}'s #{url_listing} page ##{pagenum}... "
  begin
    page = agent.get(listing_url)
  rescue Timeout::Error
    $stderr.puts "Couldn't get page #{listing_url}: #{$!.inspect} -- stopping"
    return validlinks
  rescue Mechanize::ResponseCodeError
    $stderr.puts "Couldn't get page #{listing_url}: #{$!.inspect} -- stopping"
    return validlinks
  rescue
    $stderr.puts "Couldn't get page #{listing_url}: #{$!.inspect} -- stopping"
    return validlinks
  end
  while true
    links = page.links_with(:href => /^\/view/)
    logs "No more valid links found" if links.length == 0
    break if links.length == 0
    oldlength = validlinks.length
    links.each do |link|
      validlinks[link.href] = true
    end
    numvalidlinks = validlinks.length-oldlength
    validlinks = remove_already_downloaded(db, validlinks)
    numnewlinks = validlinks.length-oldlength
    if numnewlinks == 0 and appconfig.fastscan
      logs "No more new links found"
      break
    end
    logs "Got #{numvalidlinks} valid and #{numnewlinks} new links"
    pagenum+=1
    link = page.link_with(:text => 'Next  ❯❯')
    if !link
      $stderr.puts "No more #{url_listing} pages for this artist"
      return validlinks
    end
    log_print "Going to #{artistname}'s #{url_listing} page ##{pagenum}... "
    begin
      page = link.click
    rescue Timeout::Error
      $stderr.puts "Couldn't get page #{link}: #{$!.inspect} -- stopping"
      return validlinks
    rescue Mechanize::ResponseCodeError
      $stderr.puts "Couldn't get page #{link}: #{$!.inspect} -- stopping"
      return validlinks
    rescue
      $stderr.puts "Couldn't get page #{link}: #{$!.inspect} -- stopping"
      return validlinks
    end
    page = check_and_login(agent, page)
    end
  return validlinks
end

##
## remove already downloaded page links
##
def remove_already_downloaded(db, pictures_raw)
  pictures = Hash.new
  pictures_raw.each do |key, link|
    ## get from database
    image_url, last_modified = db[key]

    next if image_url != nil
    pictures[key] = true
  end
  return pictures
end

##
## set image time
##
def setimagetime(filepath, imagetime)
  if imagetime != 0
    File.utime(File.atime(filepath), Time.at(imagetime), filepath) rescue 
    $stderr.puts "Couldn't set image #{filepath} time #{imagetime}: #{$!.inspect} -- skipping"
  end
end

##
## database functions
##
class AppDatabase
  def initialize(filepath)
    @db = SQLite3::Database.new(filepath)
    @db.busy_timeout(5000)
    @db.execute("CREATE TABLE IF NOT EXISTS image_urls (page_url TEXT PRIMARY KEY UNIQUE, image_url TEXT, last_modified TEXT)")
    @db.execute("CREATE INDEX IF NOT EXISTS page_urls ON image_urls(page_url)");
    @db.execute("PRAGMA synchronous = OFF");
    @db.execute("PRAGMA journal_mode = MEMORY");
    @db.execute("PRAGMA cache_size = 1000000");
    @db.execute("PRAGMA temp_store = MEMORY");
  end
  
  def [](image_page_url)
    result = @db.execute("SELECT image_url, last_modified FROM image_urls WHERE page_url = :page_url LIMIT 1", "page_url" => image_page_url)
    return nil if result == nil
    return nil if result.empty?
    return result[0][0], result[0][1]
  end
    
  def set_image_url(image_page_url, image_url, last_modified = nil)
    @db.execute("INSERT OR REPLACE INTO image_urls (page_url, image_url, last_modified) VALUES (:page_url, :image_url, :last_modified)",
               "page_url" => image_page_url, "image_url" => image_url, "last_modified" => last_modified)
  end
end


def downloadfrompage(key, agent, db)
  appconfig = AppConfig.instance

  # if in database, we've downloaded it already, return image filename
  log_print "1"
  begin
    dbvalue, lastmod = db[key]
    if dbvalue != nil
      filename = File.basename(dbvalue)
      logs "\b " + filename + " (already downloaded)"
      return filename
    end
  end

  ## get image page
  log_print "\b2"
  artpage_uri = "#{appconfig[:url_base]}/#{key}"
  begin
    art_page = agent.get(artpage_uri.to_s)
  rescue Timeout::Error
    $stderr.puts " Couldn't get page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  rescue
    $stderr.puts " Couldn't get page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  end

  ## get image links from the page
  log_print "\b3"
  begin
    imagelink = art_page.link_with(:text => /Download/)
    if (!imagelink)
      $stderr.puts " Got a page #{artpage_uri.to_s} without a download link -- skipping"
#      p art_page
      return nil
    end
    image_uri = Addressable::URI.parse(imagelink.href).normalize
    if image_uri.scheme == nil then image_uri.scheme = "http" end
  rescue
    $stderr.puts " Couldn't get page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  end

  ## get filename and image creation time
  log_print "\b4"
  filename = Addressable::URI.unencode_component(image_uri.basename)
  artistname = filename.scan(/^([0-9]{10}\.)([^._]+)[._]./)[0][1]
  filedir = "#{appconfig.download_directory}/#{artistname}"
  FileUtils.mkpath filedir
  filepath = "#{filedir}/#{filename}"
  imagetime = (filename.scan(/\d{10}/)[0]).to_i

  ## do HEAD request and get file size
  log_print "\b5"
  begin
    response = agent.head(image_uri.to_s)
  rescue Net::HTTPNotFound
    $stderr.puts " Couldn't get image #{image_uri.to_s} from page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  rescue
    $stderr.puts " Couldn't get image #{image_uri.to_s} from page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  end

  imagesize = response["content-length"].to_i

  ## don't download if file exists and file size matches
  log_print "\b6"
  if (File.exist?(filepath) && File.size(filepath) > 0 && File.size(filepath) == imagesize)
    logs "\b #{filename} (file exists and size matches)"
    setimagetime(filepath, imagetime)
    db.set_image_url(key, image_uri.to_s)
    return filename
  end

  ## get the image
  log_print "\b7"
  begin
    image = agent.get(image_uri.to_s)
  rescue Timeout::Error
    $stderr.puts " Couldn't get image #{image_uri.to_s} from page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  rescue
    $stderr.puts " Couldn't get image #{image_uri.to_s} from page #{artpage_uri.to_s}: #{$!.inspect} -- skipping"
    return nil
  end

  log_print "\b8"
  last_modified = image.response["Last-Modified"]
  image.save!(filepath)

  log_print "\b9"
  setimagetime(filepath, imagetime)

  log_print "\ba"
  db.set_image_url(key, image_uri.to_s, last_modified)

  logs "\b " + filename
  return filename
end
