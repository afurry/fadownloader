#!/usr/bin/env ruby

# built-in
require 'optparse'
require 'logger'

# ruby gems
require 'rubygems'

require 'keychain'

item = Keychain.add_internet_password('furaffinity.net', 'a', 'b', 'c', 'd')
puts item
p item
