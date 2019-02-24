local bot = {}
local botnick = 'testbot1'
bot.handlers = {
  ['PRIVMSG'] = function(net, nick, user, host, channel, message)
    if channel ~= botnick then return end
    if message == 'HELLO' then
      return { {command = 'PRIVMSG', params = {botnick, 'GOODBYE'}} }
    end
  end,
}
bot.servers = {
  test = {
    server = 'localhost',
    tls = false,
  },
}
bot.nick = botnick
bot.username = 'a'
bot.realname = 'e'
return bot
