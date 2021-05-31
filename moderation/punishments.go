package moderation

import (
	"context"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/jinzhu/gorm"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/dstate/v2"
	"github.com/jonas747/dutil"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/scheduledevents2"
	seventsmodels "github.com/jonas747/yagpdb/common/scheduledevents2/models"
	"github.com/jonas747/yagpdb/common/templates"
	"github.com/jonas747/yagpdb/logs"
	"github.com/mediocregopher/radix/v3"
	"github.com/volatiletech/sqlboiler/queries/qm"
)

type Punishment int

const (
	PunishmentKick Punishment = iota
	PunishmentBan
)

const (
	DefaultDMMessage = `You have been {{.ModAction}}
{{if .Reason}}**Reason:** {{.Reason}}{{end}}`
)

func getMemberWithFallback(gs *dstate.GuildState, user *discordgo.User) (ms *dstate.MemberState, notFound bool) {
	ms, err := bot.GetMember(gs.ID, user.ID)
	if err != nil {
		// Fallback
		logger.WithError(err).WithField("guild", gs.ID).Info("Failed retrieving member")
		ms = &dstate.MemberState{
			ID:       user.ID,
			Guild:    gs,
			Username: user.Username,
			Bot:      user.Bot,
		}

		parsedDiscrim, _ := strconv.ParseInt(user.Discriminator, 10, 32)
		ms.Discriminator = int32(parsedDiscrim)
		ms.ParseAvatar(user.Avatar)

		return ms, true
	}

	return ms, false
}

// Kick or bans someone, uploading a hasebin log, and sending the report message in the action channel
func punish(config *Config, p Punishment, guildID int64, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, reason string, user *discordgo.User, duration time.Duration, variadicBanDeleteDays ...int) error {

	config, err := getConfigIfNotSet(guildID, config)
	if err != nil {
		return common.ErrWithCaller(err)
	}

	var action ModlogAction
	if p == PunishmentKick {
		action = MAKick
	} else {
		action = MABanned
		if duration > 0 {
			action.Footer = "Expires after: " + common.HumanizeDuration(common.DurationPrecisionMinutes, duration)
		}
	}

	var channelID int64
	if channel != nil {
		channelID = channel.ID
	}

	gs := bot.State.Guild(true, guildID)

	member, memberNotFound := getMemberWithFallback(gs, user)
	if !memberNotFound {
		msg := config.BanMessage
		if p == PunishmentKick {
			msg = config.KickMessage
		}
		sendPunishDM(config, msg, action, gs, channel, message, author, member, duration, reason, -1)
	}

	logLink := ""
	if channelID != 0 {
		logLink = CreateLogs(guildID, channelID, author)
	}

	fullReason := reason
	if author.ID != common.BotUser.ID {
		fullReason = author.Username + "#" + author.Discriminator + ": " + reason
	}

	switch p {
	case PunishmentKick:
		err = common.BotSession.GuildMemberDeleteWithReason(guildID, user.ID, fullReason)
	case PunishmentBan:
		banDeleteDays := 1
		if len(variadicBanDeleteDays) > 0 {
			banDeleteDays = variadicBanDeleteDays[0]
		}
		err = common.BotSession.GuildBanCreateWithReason(guildID, user.ID, fullReason, banDeleteDays)
	}

	if err != nil {
		return err
	}

	logger.Infof("MODERATION: %s %s %s cause %q", author.Username, action.Prefix, user.Username, reason)

	if memberNotFound {
		// Wait a tiny bit to make sure the audit log is updated
		time.Sleep(time.Second * 3)

		auditLogType := discordgo.AuditLogActionMemberBanAdd
		if p == PunishmentKick {
			auditLogType = discordgo.AuditLogActionMemberKick
		}

		// Pull user details from audit log if we can
		auditLog, err := common.BotSession.GuildAuditLog(gs.ID, common.BotUser.ID, 0, auditLogType, 10)
		if err == nil {
			for _, v := range auditLog.Users {
				if v.ID == user.ID {
					user = &discordgo.User{
						ID:            v.ID,
						Username:      v.Username,
						Discriminator: v.Discriminator,
						Bot:           v.Bot,
						Avatar:        v.Avatar,
					}
					break
				}
			}
		}
	}

	err = CreateModlogEmbed(config, author, action, user, reason, logLink)
	return err
}

func sendPunishDM(config *Config, dmMsg string, action ModlogAction, gs *dstate.GuildState, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, member *dstate.MemberState, duration time.Duration, reason string, warningID int) {
	if dmMsg == "" {
		dmMsg = DefaultDMMessage
	}

	// Execute and send the DM message template
	ctx := templates.NewContext(gs, channel, member)
	ctx.Data["Reason"] = reason
	if duration > 0 {
		ctx.Data["Duration"] = duration
		ctx.Data["HumanDuration"] = common.HumanizeDuration(common.DurationPrecisionMinutes, duration)
	} else {
		ctx.Data["Duration"] = 0
		ctx.Data["HumanDuration"] = "never"
	}
	ctx.Data["Author"] = author
	ctx.Data["ModAction"] = action
	ctx.Data["Message"] = message

	if warningID != -1 {
		ctx.Data["WarningID"] = warningID
	}

	if duration < 1 {
		ctx.Data["HumanDuration"] = "permanently"
	}

	executed, err := ctx.Execute(dmMsg)
	if err != nil {
		logger.WithError(err).WithField("guild", gs.ID).Warn("Failed executing pusnishment DM")
		executed = "Failed executing template."
	}

	if strings.TrimSpace(executed) != "" {
		err = bot.SendDM(member.ID, "**"+bot.GuildName(gs.ID)+":** "+executed)
		if err != nil {
			logger.WithError(err).Error("failed sending punish DM")
		}
	}
}

func KickUser(config *Config, guildID int64, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, reason string, user *discordgo.User) error {
	config, err := getConfigIfNotSet(guildID, config)
	if err != nil {
		return common.ErrWithCaller(err)
	}

	err = punish(config, PunishmentKick, guildID, channel, message, author, reason, user, 0)
	if err != nil {
		return err
	}

	if !config.DeleteMessagesOnKick {
		return nil
	}

	if channel != nil {
		_, err = DeleteMessages(channel.ID, user.ID, 100, 100)
	}
	return err
}

func DeleteMessages(channelID int64, filterUser int64, deleteNum, fetchNum int) (int, error) {
	msgs, err := bot.GetMessages(channelID, fetchNum, false)
	if err != nil {
		return 0, err
	}

	toDelete := make([]int64, 0)
	now := time.Now()
	for i := len(msgs) - 1; i >= 0; i-- {
		if filterUser == 0 || msgs[i].Author.ID == filterUser {

			// Can only bulk delete messages up to 2 weeks (but add 1 minute buffer account for time sync issues and other smallies)
			if now.Sub(msgs[i].ParsedCreated) > (time.Hour*24*14)-time.Minute {
				continue
			}

			toDelete = append(toDelete, msgs[i].ID)
			//log.Println("Deleting", msgs[i].ContentWithMentionsReplaced())
			if len(toDelete) >= deleteNum || len(toDelete) >= 100 {
				break
			}
		}
	}

	if len(toDelete) < 1 {
		return 0, nil
	}

	if len(toDelete) < 1 {
		return 0, nil
	} else if len(toDelete) == 1 {
		err = common.BotSession.ChannelMessageDelete(channelID, toDelete[0])
	} else {
		err = common.BotSession.ChannelMessagesBulkDelete(channelID, toDelete)
	}

	return len(toDelete), err
}

func BanUserWithDuration(config *Config, guildID int64, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, reason string, user *discordgo.User, duration time.Duration, deleteMessageDays int) error {
	// Set a key in redis that marks that this user has appeared in the modlog already
	common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyBannedUser(guildID, user.ID), "60", "1"))
	if deleteMessageDays > 7 {
		deleteMessageDays = 7
	}
	if deleteMessageDays < 0 {
		deleteMessageDays = 0
	}

	err := punish(config, PunishmentBan, guildID, channel, message, author, reason, user, duration, deleteMessageDays)
	if err != nil {
		return err
	}

	_, err = seventsmodels.ScheduledEvents(qm.Where("event_name='moderation_unban' AND  guild_id = ? AND (data->>'user_id')::bigint = ?", guildID, user.ID)).DeleteAll(context.Background(), common.PQ)
	common.LogIgnoreError(err, "[moderation] failed clearing unban events", nil)

	if duration > 0 {
		err = scheduledevents2.ScheduleEvent("moderation_unban", guildID, time.Now().Add(duration), &ScheduledUnbanData{
			UserID: user.ID,
		})
		if err != nil {
			return errors.WithMessage(err, "punish,sched_unban")
		}
	}

	return nil
}

func BanUser(config *Config, guildID int64, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, reason string, user *discordgo.User) error {
	return BanUserWithDuration(config, guildID, channel, message, author, reason, user, 0, 1)
}

func UnbanUser(config *Config, guildID int64, author *discordgo.User, reason string, user *discordgo.User) (bool, error) {
	config, err := getConfigIfNotSet(guildID, config)
	if err != nil {
		return false, common.ErrWithCaller(err)
	}
	action := MAUnbanned

	//Delete all future Unban Events
	_, err = seventsmodels.ScheduledEvents(qm.Where("event_name='moderation_unban' AND  guild_id = ? AND (data->>'user_id')::bigint = ?", guildID, user.ID)).DeleteAll(context.Background(), common.PQ)
	common.LogIgnoreError(err, "[moderation] failed clearing unban events", nil)

	//We need details for user only if unban is to be logged in modlog. Thus we can save a potential api call by directly attempting an unban in other cases.
	if config.LogUnbans && config.IntActionChannel() != 0 {
		// check if they're already banned
		guildBan, err := common.BotSession.GuildBan(guildID, user.ID)
		if err != nil {
			notbanned, err := isNotFound(err)
			return notbanned, err
		}
		user = guildBan.User
	}

	// Set a key in redis that marks that this user has appeared in the modlog already
	common.RedisPool.Do(radix.FlatCmd(nil, "SETEX", RedisKeyUnbannedUser(guildID, user.ID), 30, 2))

	err = common.BotSession.GuildBanDelete(guildID, user.ID)
	if err != nil {
		notbanned, err := isNotFound(err)
		return notbanned, err
	}

	logger.Infof("MODERATION: %s %s %s cause %q", author.Username, action.Prefix, user.Username, reason)

	//modLog Entry handling
	if config.LogUnbans {
		err = CreateModlogEmbed(config, author, action, user, reason, "")
	}
	return false, err
}

func isNotFound(err error) (bool, error) {
	if err != nil {
		if cast, ok := err.(*discordgo.RESTError); ok && cast.Response != nil {
			if cast.Response.StatusCode == 404 {
				return true, nil // Not found
			}
		}
		return false, err
	}
	return false, nil
}

const (
	ErrNoMuteRole = errors.Sentinel("No mute role")
)

// Unmut or mute a user, ignore duration if unmuting
// TODO: i don't think we need to track mutes in its own database anymore now with the new scheduled event system
func MuteUnmuteUser(config *Config, mute bool, guildID int64, channel *dstate.ChannelState, message *discordgo.Message, author *discordgo.User, reason string, member *dstate.MemberState, duration int) error {
	config, err := getConfigIfNotSet(guildID, config)
	if err != nil {
		return common.ErrWithCaller(err)
	}

	if config.MuteRole == "" {
		return ErrNoMuteRole
	}

	var channelID int64
	if channel != nil {
		channelID = channel.ID
	}

	// To avoid unexpected things from happening, make sure were only updating the mute of the player 1 place at a time
	LockMute(member.ID)
	defer UnlockMute(member.ID)

	// Look for existing mute
	currentMute := MuteModel{}
	err = common.GORM.Where(&MuteModel{UserID: member.ID, GuildID: guildID}).First(&currentMute).Error
	alreadyMuted := err != gorm.ErrRecordNotFound
	if err != nil && err != gorm.ErrRecordNotFound {
		return common.ErrWithCaller(err)
	}

	// Insert/update the mute entry in the database
	if !alreadyMuted {
		currentMute = MuteModel{
			UserID:  member.ID,
			GuildID: guildID,
		}
	}

	if author != nil {
		currentMute.AuthorID = author.ID
	}

	currentMute.Reason = reason
	if duration > 0 {
		currentMute.ExpiresAt = time.Now().Add(time.Minute * time.Duration(duration))
	}

	// no matter what, if were unmuting or muting, we wanna make sure we dont have duplicated unmute events
	_, err = seventsmodels.ScheduledEvents(qm.Where("event_name='moderation_unmute' AND  guild_id = ? AND (data->>'user_id')::bigint = ?", guildID, member.ID)).DeleteAll(context.Background(), common.PQ)
	common.LogIgnoreError(err, "[moderation] failed clearing unban events", nil)

	if mute {
		// Apply the roles to the user
		removedRoles, err := AddMemberMuteRole(config, member.ID, member.Roles)
		if err != nil {
			return errors.WithMessage(err, "AddMemberMuteRole")
		}

		if alreadyMuted {
			// Append new removed roles to the removed_roles array
		OUTER:
			for _, removedNow := range removedRoles {
				for _, alreadyRemoved := range currentMute.RemovedRoles {
					if removedNow == alreadyRemoved {
						continue OUTER
					}
				}

				// Not in the removed slice
				currentMute.RemovedRoles = append(currentMute.RemovedRoles, removedNow)
			}
		} else {
			// New mute, so can just do whatever
			currentMute.RemovedRoles = removedRoles
		}

		err = common.GORM.Save(&currentMute).Error
		if err != nil {
			return errors.WithMessage(err, "failed inserting/updating mute")
		}

		if duration > 0 {
			err = scheduledevents2.ScheduleEvent("moderation_unmute", guildID, time.Now().Add(time.Minute*time.Duration(duration)), &ScheduledUnmuteData{
				UserID: member.ID,
			})
			if err != nil {
				return errors.WithMessage(err, "failed scheduling unmute")
			}
		}
	} else {
		// Remove the mute role, and give back the role the bot took
		err = RemoveMemberMuteRole(config, member.ID, member.Roles, currentMute)
		if err != nil {
			return errors.WithMessage(err, "failed removing mute role")
		}

		if alreadyMuted {
			common.GORM.Delete(&currentMute)
			common.RedisPool.Do(radix.Cmd(nil, "DEL", RedisKeyMutedUser(guildID, member.ID)))
		}
	}

	// Upload logs
	logLink := ""
	if channelID != 0 && mute {
		logLink = CreateLogs(guildID, channelID, author)
	}

	dmMsg := config.UnmuteMessage
	action := MAUnmute
	if mute {
		action = MAMute
		action.Footer = "Duration: "
		if duration > 0 {
			action.Footer += common.HumanizeDuration(common.DurationPrecisionMinutes, time.Duration(duration)*time.Minute)
		} else {
			action.Footer += "permanent"
		}
		dmMsg = config.MuteMessage
	}

	gs := bot.State.Guild(true, guildID)
	if gs != nil {
		sendPunishDM(config, dmMsg, action, gs, channel, message, author, member, time.Duration(duration)*time.Minute, reason, -1)
	}

	// Create the modlog entry
	return CreateModlogEmbed(config, author, action, member.DGoUser(), reason, logLink)
}

func AddMemberMuteRole(config *Config, id int64, currentRoles []int64) (removedRoles []int64, err error) {
	removedRoles = make([]int64, 0, len(config.MuteRemoveRoles))
	newMemberRoles := make([]string, 0, len(currentRoles))
	newMemberRoles = append(newMemberRoles, config.MuteRole)

	hadMuteRole := false
	for _, r := range currentRoles {
		if config.IntMuteRole() == r {
			hadMuteRole = true
			continue
		}

		if common.ContainsInt64Slice(config.MuteRemoveRoles, r) {
			removedRoles = append(removedRoles, r)
		} else {
			newMemberRoles = append(newMemberRoles, strconv.FormatInt(r, 10))
		}
	}

	if hadMuteRole && len(removedRoles) < 1 {
		// No changes needs to be made
		return
	}

	err = common.BotSession.GuildMemberEdit(config.GuildID, id, newMemberRoles)
	return
}

func RemoveMemberMuteRole(config *Config, id int64, currentRoles []int64, mute MuteModel) (err error) {
	newMemberRoles := decideUnmuteRoles(config, currentRoles, mute)
	err = common.BotSession.GuildMemberEdit(config.GuildID, id, newMemberRoles)
	return
}

func decideUnmuteRoles(config *Config, currentRoles []int64, mute MuteModel) []string {
	newMemberRoles := make([]string, 0)

	gs := bot.State.Guild(true, config.GuildID)
	botState, err := bot.GetMember(gs.ID, common.BotUser.ID)

	gs.RLock()
	defer gs.RUnlock()

	guildRoles := make([]int64, len(gs.Guild.Roles))
	for k, e := range gs.Guild.Roles {
		guildRoles[k] = e.ID
	}

	if err != nil || botState == nil { // We couldn't find the bot on state, so keep old behaviour
		for _, r := range currentRoles {
			if r != config.IntMuteRole() {
				newMemberRoles = append(newMemberRoles, strconv.FormatInt(r, 10))
			}
		}

		for _, r := range mute.RemovedRoles {
			if !common.ContainsInt64Slice(currentRoles, r) && common.ContainsInt64Slice(guildRoles, r) {
				newMemberRoles = append(newMemberRoles, strconv.FormatInt(r, 10))
			}
		}

		return newMemberRoles
	}

	yagHighest := bot.MemberHighestRole(gs, botState)

	for _, v := range currentRoles {
		if v != config.IntMuteRole() {
			newMemberRoles = append(newMemberRoles, strconv.FormatInt(v, 10))
		}
	}

	for _, v := range mute.RemovedRoles {
		if !common.ContainsInt64Slice(currentRoles, v) && common.ContainsInt64Slice(guildRoles, v) && dutil.IsRoleAbove(yagHighest, gs.Role(false, v)) {
			newMemberRoles = append(newMemberRoles, strconv.FormatInt(v, 10))
		}
	}

	return newMemberRoles
}

func WarnUser(config *Config, guildID int64, channel *dstate.ChannelState, msg *discordgo.Message, author *discordgo.User, target *discordgo.User, message string) error {
	warning := &WarningModel{
		GuildID:               guildID,
		UserID:                discordgo.StrID(target.ID),
		AuthorID:              discordgo.StrID(author.ID),
		AuthorUsernameDiscrim: author.Username + "#" + author.Discriminator,

		Message: message,
	}

	var channelID int64
	if channel != nil {
		channelID = channel.ID
	}

	config, err := getConfigIfNotSet(guildID, config)
	if err != nil {
		return common.ErrWithCaller(err)
	}

	if config.WarnIncludeChannelLogs && channelID != 0 {
		warning.LogsLink = CreateLogs(guildID, channelID, author)
	}

	// Create the entry in the database
	err = common.GORM.Create(warning).Error
	if err != nil {
		return common.ErrWithCaller(err)
	}

	gs := bot.State.Guild(true, guildID)
	ms, _ := bot.GetMember(guildID, target.ID)
	if gs != nil && ms != nil {
		sendPunishDM(config, config.WarnMessage, MAWarned, gs, channel, msg, author, ms, -1, message, int(warning.ID))
	}

	// go bot.SendDM(target.ID, fmt.Sprintf("**%s**: You have been warned for: %s", bot.GuildName(guildID), message))

	if config.WarnSendToModlog && config.ActionChannel != "" {
		err = CreateModlogEmbed(config, author, MAWarned, target, message, warning.LogsLink)
		if err != nil {
			return common.ErrWithCaller(err)
		}
	}

	return nil
}

func CreateLogs(guildID, channelID int64, user *discordgo.User) string {
	lgs, err := logs.CreateChannelLog(context.TODO(), nil, guildID, channelID, user.Username, user.ID, 100)
	if err != nil {
		if err == logs.ErrChannelBlacklisted {
			return ""
		}
		logger.WithError(err).Error("Log Creation Failed")
		return "Log Creation Failed"
	}
	return logs.CreateLink(guildID, lgs.ID)
}