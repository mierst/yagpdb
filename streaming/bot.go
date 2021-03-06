package streaming

import (
	"github.com/jonas747/discordgo"
	"github.com/jonas747/dutil/dstate"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/bot/eventsystem"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/pubsub"
	"github.com/jonas747/yagpdb/common/templates"
	"github.com/mediocregopher/radix.v2/redis"
	log "github.com/sirupsen/logrus"
	"regexp"
	"strings"
	"sync"
)

func KeyCurrentlyStreaming(gID int64) string { return "currently_streaming:" + discordgo.StrID(gID) }

var _ bot.BotInitHandler = (*Plugin)(nil)
var _ bot.ShardMigrationHandler = (*Plugin)(nil)

func (p *Plugin) BotInit() {
	eventsystem.AddHandler(bot.ConcurrentEventHandler(bot.RedisWrapper(HandleGuildCreate)), eventsystem.EventGuildCreate)
	eventsystem.AddHandler(bot.RedisWrapper(HandlePresenceUpdate), eventsystem.EventPresenceUpdate)
	eventsystem.AddHandler(bot.RedisWrapper(HandleGuildMemberUpdate), eventsystem.EventGuildMemberUpdate)
	pubsub.AddHandler("update_streaming", HandleUpdateStreaming, nil)
}

func (p *Plugin) GuildMigrated(gs *dstate.GuildState, toThisSlave bool) {
	if !toThisSlave {
		return
	}

	go CheckGuildFull(gs)
}

// YAGPDB event
func HandleUpdateStreaming(event *pubsub.Event) {
	log.Info("Received update streaming event ", event.TargetGuild)

	gs := bot.State.Guild(true, event.TargetGuildInt)
	if gs == nil {
		return
	}

	CheckGuildFull(gs)
}

func CheckGuildFull(gs *dstate.GuildState) {
	log.Info("Streaming Checking full guild: ", gs.ID)

	client, err := common.RedisPool.Get()
	if err != nil {
		log.WithError(err).Error("Failed retrieving redis connection from pool")
		return
	}
	defer common.RedisPool.Put(client)

	config, err := GetConfig(client, gs.ID)
	if err != nil {
		log.WithError(err).WithField("guild", gs.ID).Error("Failed retrieving streaming config")
	}

	gs.RLock()

	var wg sync.WaitGroup

	slowCheck := make([]*dstate.MemberState, 0, len(gs.Members))
	for _, ms := range gs.Members {

		if !ms.MemberSet || !ms.PresenceSet {
			if ms.PresenceSet {
				slowCheck = append(slowCheck, ms)
				wg.Add(1)
				go func(gID, uID int64) {
					bot.GetMember(gID, uID)
					wg.Done()

				}(gs.ID, ms.ID)
			}

			continue
		}

		err = CheckPresence(client, config, ms, gs)
		if err != nil {
			log.WithError(err).Error("Error checking presence")
			continue
		}
	}

	gs.RUnlock()

	wg.Wait()

	log.WithField("guild", gs.ID).Info("Starting slowcheck")
	gs.RLock()
	for _, ms := range slowCheck {

		if !ms.MemberSet || !ms.PresenceSet {
			continue
		}

		err = CheckPresence(client, config, ms, gs)
		if err != nil {
			log.WithError(err).Error("Error checking presence")
			continue
		}
	}
	gs.RUnlock()
	log.WithField("guild", gs.ID).Info("Done slowcheck")
}

func HandleGuildMemberUpdate(evt *eventsystem.EventData) {
	client := bot.ContextRedis(evt.Context())
	m := evt.GuildMemberUpdate()

	config, err := GetConfig(client, m.GuildID)
	if err != nil {
		log.WithError(err).Error("Failed retrieving streaming config")
		return
	}

	gs := bot.State.Guild(true, m.GuildID)
	if gs == nil {
		log.WithField("guild", m.GuildID).Error("Guild not found in state")
		return
	}

	ms := gs.Member(true, m.User.ID)
	if ms == nil {
		log.WithField("guild", m.GuildID).Error("Member not found in state")
		return
	}

	gs.RLock()
	defer gs.RUnlock()

	if !ms.PresenceSet {
		log.WithField("guild", m.GuildID).Warn("Presence not found in state")
		return
	}

	err = CheckPresence(client, config, ms, gs)
	if err != nil {
		log.WithError(err).Error("Failed checking presence")
	}
}

func HandleGuildCreate(evt *eventsystem.EventData) {

	client := bot.ContextRedis(evt.Context())
	g := evt.GuildCreate()

	config, err := GetConfig(client, g.ID)
	if err != nil {
		log.WithError(err).Error("Failed retrieving streaming config")
		return
	}

	gs := bot.State.Guild(true, g.ID)
	if gs == nil {
		log.WithField("guild", g.ID).Error("Guild not found in state")
		return
	}

	gs.RLock()
	defer gs.RUnlock()

	for _, ms := range gs.Members {

		if !ms.MemberSet || !ms.PresenceSet {
			continue
		}

		err = CheckPresence(client, config, ms, gs)

		if err != nil {
			log.WithError(err).Error("Failed checking presence")
		}
	}
}

func HandlePresenceUpdate(evt *eventsystem.EventData) {
	client := bot.ContextRedis(evt.Context())
	p := evt.PresenceUpdate()

	config, err := GetConfig(client, p.GuildID)
	if err != nil {
		log.WithError(err).Error("Failed retrieving streaming config")
		return
	}

	if !config.Enabled {
		return
	}

	gs := bot.State.Guild(true, p.GuildID)
	if gs == nil {
		log.WithField("guild", p.GuildID).Error("Failed retrieving guild from state")
		return
	}

	ms, err := bot.GetMember(p.GuildID, p.User.ID)
	if ms == nil || err != nil {
		log.WithError(err).WithField("guild", p.GuildID).WithField("user", p.User.ID).Error("Failed retrieving member")
		return
	}

	gs.RLock()
	defer gs.RUnlock()

	err = CheckPresence(client, config, ms, gs)
	if err != nil {
		log.WithError(err).WithField("guild", p.GuildID).Error("Failed checking presence")
	}
}

func CheckPresence(client *redis.Client, config *Config, ms *dstate.MemberState, gs *dstate.GuildState) error {
	if !config.Enabled {
		// RemoveStreaming(client, config, gs.ID, p.User.ID, member)
		return nil
	}

	// Now the real fun starts
	// Either add or remove the stream
	if ms.PresenceStatus != dstate.StatusOffline && ms.PresenceGame != nil && ms.PresenceGame.URL != "" {
		// Streaming

		if !config.MeetsRequirements(ms) {
			RemoveStreaming(client, config, gs.ID, ms)
			return nil
		}

		if config.GiveRole != 0 {
			err := GiveStreamingRole(ms, config.GiveRole, gs.Guild)
			if err != nil && !common.IsDiscordErr(err, discordgo.ErrCodeMissingPermissions, discordgo.ErrCodeUnknownRole) {
				log.WithError(err).WithField("guild", gs.ID).WithField("user", ms.ID).Error("Failed adding streaming role")
				client.Cmd("SREM", KeyCurrentlyStreaming(gs.ID), ms.ID)
			}
		}

		// Was already marked as streaming before if we added 0 elements
		if num, _ := client.Cmd("SADD", KeyCurrentlyStreaming(gs.ID), ms.ID).Int(); num == 0 {
			return nil
		}

		// Send the streaming announcement if enabled
		if config.AnnounceChannel != 0 && config.AnnounceMessage != "" {
			SendStreamingAnnouncement(client, config, gs, ms)
		}

	} else {
		// Not streaming
		RemoveStreaming(client, config, gs.ID, ms)
	}

	return nil
}

func (config *Config) MeetsRequirements(ms *dstate.MemberState) bool {
	// Check if they have the required role
	if config.RequireRole != 0 {
		if !common.ContainsInt64Slice(ms.Roles, config.RequireRole) {
			// Dosen't have required role
			return false
		}
	}

	// Check if they have a ignored role
	if config.IgnoreRole != 0 {
		if common.ContainsInt64Slice(ms.Roles, config.IgnoreRole) {
			// We ignore people with this role.. :'(
			return false
		}
	}

	if strings.TrimSpace(config.GameRegex) != "" {
		gameName := ms.PresenceGame.Details
		compiledRegex, err := regexp.Compile(strings.TrimSpace(config.GameRegex))
		if err == nil {
			// It should be verified before this that its valid
			if !compiledRegex.MatchString(gameName) {
				return false
			}
		}
	}

	if strings.TrimSpace(config.TitleRegex) != "" {
		streamTitle := ms.PresenceGame.Name
		compiledRegex, err := regexp.Compile(strings.TrimSpace(config.TitleRegex))
		if err == nil {
			// It should be verified before this that its valid
			if !compiledRegex.MatchString(streamTitle) {
				return false
			}
		}
	}

	return true
}

func RemoveStreaming(client *redis.Client, config *Config, guildID int64, ms *dstate.MemberState) {
	if ms.MemberSet {
		client.Cmd("SREM", KeyCurrentlyStreaming(guildID), ms.ID)
		go RemoveStreamingRole(ms, config.GiveRole, guildID)
	} else {
		// Was not streaming before if we removed 0 elements
		if n, _ := client.Cmd("SREM", KeyCurrentlyStreaming(guildID), ms.ID).Int(); n > 0 && config.GiveRole != 0 {
			go common.BotSession.GuildMemberRoleRemove(guildID, ms.ID, config.GiveRole)
		}
	}
}

func SendStreamingAnnouncement(client *redis.Client, config *Config, guild *dstate.GuildState, ms *dstate.MemberState) {
	foundChannel := false
	for _, v := range guild.Channels {
		if v.ID == config.AnnounceChannel {
			foundChannel = true
		}
	}

	if !foundChannel {
		log.WithField("guild", guild.ID).WithField("channel", config.AnnounceChannel).Warn("Channel not found in state, not sending streaming announcement")
		return
	}

	ctx := templates.NewContext(guild, nil, ms)
	ctx.Data["URL"] = common.EscapeSpecialMentions(ms.PresenceGame.URL)
	ctx.Data["url"] = common.EscapeSpecialMentions(ms.PresenceGame.URL)
	ctx.Data["Game"] = ms.PresenceGame.Details
	ctx.Data["StreamTitle"] = ms.PresenceGame.Name

	guild.RUnlock()
	out, err := ctx.Execute(client, config.AnnounceMessage)
	guild.RLock()
	if err != nil {
		log.WithError(err).WithField("guild", guild.ID).Warn("Failed executing template")
		return
	}

	common.BotSession.ChannelMessageSend(config.AnnounceChannel, out)
}

func GiveStreamingRole(ms *dstate.MemberState, role int64, guild *discordgo.Guild) error {
	// Ensure the role exists
	found := false
	for _, v := range guild.Roles {
		if v.ID == role {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	go common.AddRoleDS(ms, role)
	return nil
}

func RemoveStreamingRole(ms *dstate.MemberState, role int64, guildID int64) {
	if role == 0 {
		return
	}

	err := common.RemoveRoleDS(ms, role)
	if err != nil && !common.IsDiscordErr(err, discordgo.ErrCodeMissingPermissions, discordgo.ErrCodeUnknownRole, discordgo.ErrCodeMissingAccess) {
		log.WithError(err).WithField("guild", guildID).WithField("user", ms.ID).WithField("role", role).Error("Failed removing streaming role")
	}
}
