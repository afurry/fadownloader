#!/usr/bin/env ruby

# gem install mechanize sqlite3

# ruby gems
require 'rubygems'
require 'mechanize'
require 'yaml'
require 'time'
require 'singleton'
require 'sqlite3'
require 'xdg'

def logs(string)
  puts string
end

def log_print(string)
  print string
  $stdout.flush
end


class AppConfig
  include Singleton

  def initialize
    @data = {}
    self.url_base = 'https://inkbunny.net/'

    @data[:program_identity_short] = "ibdownloader" # used for settings dir on linux
    @data[:program_identity_lean] = "IB Downloader" # used for settings dir on Mac/Windows
    @data[:program_identity_lean_nospace] = "IBDownloader" # used for settings dir on Mac/Windows

    #####################
    ## Settings/Options
    ## Different paths for linux/macosx/windows
    #####################
    case RbConfig::CONFIG['host_os']
    when /darwin/i
      # MacOSX
      self.settings_directory = File.expand_path("~/Library/Application Support/#{@data[:program_identity_lean]}")
    else
      # Generic unix
      self.settings_directory = File.expand_path("#{XDG['CONFIG_HOME']}/#{@data[:program_identity_short]}")
    end

    self.download_directory = File.expand_path("~/Pictures/#{@data[:program_identity_lean_nospace]}")
  end


  def [](key)
    @data[key.to_sym]
  end
  
  ## user visible settings
  # attr_accessor :verbose
  # attr_accessor :gallery
  # attr_accessor :favourites
  # attr_accessor :scraps
  # attr_accessor :fastscan
  attr_accessor :download_directory

  ## internal variables
  attr_accessor :username
  attr_accessor :password
  attr_accessor :url_base
  attr_accessor :cookies_filepath, :database_filepath, :config_filepath, :settings_directory

  def loadconfig
    file_contents = YAML::load_file(self.config_filepath)
    raise "Config file #{self.config_filepath} is empty" if file_contents == false
    loaded = Hash[file_contents.map { |k, v| [k.to_sym, v] }]
    raise "No username is set in config file" if loaded[:username] == nil
    raise "No password is set in config file" if loaded[:password] == nil
    self.username = loaded[:username]
    self.password = loaded[:password]
  end
  
  def username=(value)
    @username = value
    self.cookies_filepath = File.expand_path("#{self.settings_directory}/cookies.#{value}.txt")
  end

  def settings_directory=(value)
    @settings_directory = value
    self.cookies_filepath = File.expand_path("#{value}/cookies.txt")
    self.database_filepath = File.expand_path("#{value}/downloaded.sqlite")
    self.config_filepath = File.expand_path("#{value}/config.yaml")
  end
end

class AppDatabase
  def initialize(filepath)
    @db = SQLite3::Database.new(filepath)
    @db.busy_timeout(5000)
    @db.execute("CREATE TABLE IF NOT EXISTS image_urls (page_url TEXT PRIMARY KEY UNIQUE, image_url TEXT)")
  end
  
  def [](image_page_url)
    result = @db.execute("SELECT page_url, image_url FROM image_urls WHERE page_url = :page_url LIMIT 1", "page_url" => image_page_url)
    return nil if result == nil
    return nil if result.empty?
    return result[0][0], result[0][1]
  end
    
  def remember_picture(image_page_url, image_url)
    @db.execute("INSERT OR REPLACE INTO image_urls (page_url, image_url) VALUES (:page_url, :image_url)",
               "page_url" => image_page_url, "image_url" => image_url)
  end

  def remove_already_downloaded(pictures_raw)
    pictures = Array.new
    pictures_raw.each do |value|
      ## get from database
      next if self[value.href] != nil
      pictures << value
    end
    return pictures
  end
end

app = AppConfig.instance
agent = Mechanize.new
agent.max_history = 0
agent.user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_8_3) AppleWebKit/536.28.10 (KHTML, like Gecko) Version/6.0.3 Safari/536.28.10"

FileUtils.mkpath app.settings_directory

begin
  app.loadconfig
rescue
  $stderr.puts "Couldn't load configuration -- #{$!.inspect}"
  $stderr.puts ""
  $stderr.puts "Please create a file '#{app.config_filepath}' with contents like this:"
  $stderr.puts "username: <your username>"
  $stderr.puts "password: <your password>"
  $stderr.puts ""
  $stderr.puts "And run this program again"
  exit 1
end

## TODO: command line arguments
db = AppDatabase.new app.database_filepath
FileUtils.mkpath app.download_directory

## load cookies
logs "Loading cookies"
agent.cookie_jar.load(app.cookies_filepath, :cookiestxt) if File.exists?(app.cookies_filepath)

## main site
logs "Opening main page"
page = agent.get(app.url_base)

## login
def do_login(app, agent, page)
  return page if is_logged_in?(app, agent, page)
  logs "Looking for login link"
  link = page.link_with!(:href => 'login.php')

  logs "Clicking login link"
  page = link.click

  logs "Looking for login form and filling it"
  form = page.form_with(:action => 'login_process.php')
  form.field_with(:name => "username").value = app.username
  form.field_with(:name => "password").value = app.password

  logs "Looking for login button"
  button = form.button_with(:value => 'Login')

  logs "Pressing login button"
  page = form.click_button(button)
  # page = agent.submit(form, button)

  logs "Saving cookies"
  agent.cookie_jar.save_as(app.cookies_filepath, :cookiestxt)
end

def is_logged_in?(app, agent, page)
  logs "Checking if we are logged in"
  ## TODO: check if we are on inkbunny

  link = page.link_with(:href => 'login.php')
  return false if link

  link = page.link_with(:text => 'Logout')
  return true if link

  ## TODO: throw an exception
  raise "We are neither logged in or logged out"
end

logs "Logging in if necessary"
page = do_login(app, agent, page)

pictures = Array.new

## go over every artist
ARGV.each do |artist|
  page = agent.get("#{app.url_base}/#{artist}")
  gallery_link = page.link_with(href: /^usergallery_process\.php/, text: 'Gallery')

  ## iterate over gallery pages and remember pictures
  while gallery_link
    page_num = 1
    params = CGI.parse(URI.parse(gallery_link.href).query)
    page_num = params["page"][0] if (params["page"][0] != nil)
    log_print "Going to #{artist}'s gallery page ##{page_num}"
    # logs
    page = gallery_link.click
    page_pictures = Array.new
    page.links_with(href: /^submissionview\.php/).each do |link|
      page_pictures << link
    end
    numlinks = page_pictures.length
    page_pictures = db.remove_already_downloaded(page_pictures)
    numnewlinks = page_pictures.length
    if numnewlinks == 0
      logs " - No more new links found"
      break
    end
    pictures = pictures + page_pictures
    logs " - Got #{numlinks} links and #{numnewlinks} new links"
    gallery_link = page.link_with(href: /^submissionsviewall\.php/, text: /Next Page/)
  end
end

## download pictures
pictures.each do |orig_link|
  link = orig_link
  next if db[link.href] != nil ## skip already downloaded picture
  while link
    log_print "Going to image page #{link.uri}"
    page = link.click
    image_link = page.link_with(href: %r{/files/full/}, text: /max\.? *preview|download/i)
    if image_link == nil
      image_link = page.image_with!(src: %r{/files/screen/})
      image_url = image_link.src
    else
      image_url = image_link.href
    end
    log_print " - downloading image #{image_url}"

    filename = File.basename(image_url)
    filepath = "#{app.download_directory}/#{filename}"
    if (!File.exist?(filepath) || File.size(filepath) == 0)
      logs " - saving"
      image = agent.download(image_url, filepath, [], page.uri)
      ## set file time
      last_modified = Time.parse(image.response["Last-Modified"])
      File.utime(last_modified, last_modified, filepath)
    else
      logs " - already downloaded"
    end

    db.remember_picture(link.href, image_url)

    link = page.link_with(href: /^#{Regexp.escape(link.href)}/, text: /next/i);
  end
end

