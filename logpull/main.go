// Package logpull contains functions related to downloading historical data.
package logpull

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/tsudoko/pullcord/logcache"
	"github.com/tsudoko/pullcord/logentry"
	"github.com/tsudoko/pullcord/logutil"
	"github.com/tsudoko/pullcord/tsv"
)

type Puller struct {
	d *discordgo.Session

	log *os.File

	// not fully implemented yet, we currently don't check if all emoji/attachments/etc with log entries have been downloaded
	lightMode bool // if true, attachments, emoji, icons, etc. aren't downloaded

	cache   logcache.Entries // for tracking changes between different pulls
	ever    logcache.IDs     // for determining if there's a need to add an entry for an external entity, i.e. a user who left
	deleted logcache.IDs     // for tracking deletions between different pulls, cache could be used for that as well
}

func NewPuller(d *discordgo.Session, gid string) (*Puller, error) {
	p := &Puller{d: d}

	if err := p.openLog(gid); err != nil {
		return nil, fmt.Errorf("error opening the log file: %v", err)
	}

	if err := p.loadCaches(); err != nil {
		return nil, fmt.Errorf("error reconstructing guild state: %v", err)
	}

	return p, nil
}

func (p *Puller) Close() error {
	return p.log.Close()
}

func (p *Puller) openLog(id string) error {
	if p.log != nil {
		return nil
	}

	filename := fmt.Sprintf("channels/%s/guild.tsv", id)
	if err := os.MkdirAll(path.Dir(filename), os.ModeDir|0755); err != nil {
		return errors.New("creating the guild dir failed")
	}

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	p.log = f

	return err
}

func (p *Puller) loadCaches() error {
	if p.cache != nil && p.ever != nil && p.deleted != nil {
		return nil
	}

	if p.log == nil {
		return errors.New("log file uninitialized")
	}

	p.cache = make(logcache.Entries)
	p.ever = make(logcache.IDs)

	if err := logcache.NewEntries(p.log.Name(), &p.cache); err != nil {
		return err
	}

	if err := logutil.AllIDs(p.log.Name(), &p.ever); err != nil {
		return err
	}

	p.deleted = p.cache.IDs()

	return nil
}

func (p *Puller) PullGuild(id string) error {
	guild, err := p.d.Guild(id)
	if err != nil {
		return fmt.Errorf("error getting guild info: %v", err)
	}

	if guild.Icon != "" {
		err := p.cdnDL(guild, cdnIcon)
		if err != nil {
			return fmt.Errorf("error downloading the guild icon: %v", err)
		}
	}

	if guild.Splash != "" {
		err := p.cdnDL(guild, cdnSplash)
		if err != nil {
			return fmt.Errorf("error downloading the guild splash: %v", err)
		}
	}

	p.cache.WriteNew(p.log, logentry.Make("history", "add", guild))
	delete(p.deleted[logentry.Type(guild)], guild.ID)

	gch, err := p.d.GuildChannels(guild.ID)
	if err != nil {
		return fmt.Errorf("error getting channels: %v", err)
	}

	for _, c := range gch {
		p.cache.WriteNew(p.log, logentry.Make("history", "add", c))
		delete(p.deleted[logentry.Type(c)], c.ID)

		// permission overwrite IDs are not unique right now
		// we could concatenate the channel ID with the role/user ID, but that would make the ID 128-bit wide
		/*
			for _, o := range c.PermissionOverwrites {
				p.cache.WriteNew(p.log, logentry.Make("history", "add", o))
				delete(p.deleted[logentry.Type(o)], o.ID)
			}
		*/
	}

	for _, r := range guild.Roles {
		p.cache.WriteNew(p.log, logentry.Make("history", "add", r))
		delete(p.deleted[logentry.Type(r)], r.ID)
	}

	for _, e := range guild.Emojis {
		err := p.cdnDL(e, 0)
		if err != nil {
			return fmt.Errorf("error downloading emoji %s: %v", e.ID, err)
		}
		p.cache.WriteNew(p.log, logentry.Make("history", "add", e))
		delete(p.deleted[logentry.Type(e)], e.ID)
	}

	after := "0"
	for {
		members, err := p.d.GuildMembers(id, after, 1000)
		if err != nil {
			return fmt.Errorf("error getting members from %s: %v", after, err)
		}

		if len(members) == 0 {
			break
		}

		for _, m := range members {
			after = m.User.ID
			if err := p.pullMember(m); err != nil {
				return err
			}
		}

		log.Printf("[%s] downloaded %d members, last id %s with name %s", id, len(members), after, members[len(members)-1].User.Username)
	}

	p.log.Sync()

	for etype, ids := range p.deleted {
		for id := range ids {
			entry := p.cache[etype][id]
			entry[logentry.HTime] = logentry.Timestamp()
			entry[logentry.HOp] = "del"
			tsv.Write(p.log, entry)
		}
	}

	return nil
}

func (p *Puller) pullMember(m *discordgo.Member) error {
	if m.User.Avatar != "" {
		err := p.cdnDL(m.User, cdnAvatar)
		if err != nil {
			return fmt.Errorf("error downloading avatar for user %s: %v", m.User.ID, err)
		}
	}

	if p.ever["member"] == nil {
		p.ever["member"] = make(map[string]bool)
	}
	p.ever["member"][m.User.ID] = true

	p.cache.WriteNew(p.log, logentry.Make("history", "add", m))
	delete(p.deleted[logentry.Type(m)], m.User.ID)

	return nil
}

func (p *Puller) PullDMGuild() error {
	chans, err := p.d.UserChannels()
	if err != nil {
		return fmt.Errorf("error getting DM channels: %v", err)
	}

	for _, c := range chans {
		p.cache.WriteNew(p.log, logentry.Make("history", "add", c))
		delete(p.deleted[logentry.Type(c)], c.ID)
		for _, r := range c.Recipients {
			m := &discordgo.Member{User: r}
			if err := p.pullMember(m); err != nil {
				return err
			}
		}

		// permission overwrite IDs are not unique right now
		// we could concatenate the channel ID with the role/user ID, but that would make the ID 128-bit wide
		/*
			for _, o := range c.PermissionOverwrites {
				p.cache.WriteNew(p.log, logentry.Make("history", "add", o))
				delete(p.deleted[logentry.Type(o)], o.ID)
			}
		*/
	}

	p.log.Sync()

	for etype, ids := range p.deleted {
		for id := range ids {
			entry := p.cache[etype][id]
			entry[logentry.HTime] = logentry.Timestamp()
			entry[logentry.HOp] = "del"
			tsv.Write(p.log, entry)
		}
	}

	return nil
}

func (p *Puller) PullChannel(c *discordgo.Channel) error {
	after := "0"
	filename := fmt.Sprintf("channels/%s/%s.tsv", c.GuildID, c.ID)

	if _, err := os.Stat(filename); err == nil {
		after, err = logutil.LastMessageID(filename)
		if err != nil {
			return fmt.Errorf("error getting last message id: %v", err)
		}
	}

	if c.Icon != "" {
		err := p.cdnDL(c, cdnChannelIcon)
		if err != nil {
			return fmt.Errorf("error downloading channel icon: %v", err)
		}
	}

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("error opening the log file: %v", err)
	}
	defer f.Close()

	for {
		msgs, err := p.d.ChannelMessages(c.ID, 100, "", after, "")
		if err != nil {
			return fmt.Errorf("error getting messages from %s: %v", after, err)
		}

		if len(msgs) == 0 {
			break
		}

		after = msgs[0].ID

		// messages are retrieved in descending order
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Type != discordgo.MessageTypeDefault {
				continue
			}

			if p.ever["member"] == nil {
				p.ever["member"] = make(map[string]bool)
			}

			if !p.ever["member"][msgs[i].Author.ID] {
				member := &discordgo.Member{User: msgs[i].Author}

				if member.User.Avatar != "" {
					err := p.cdnDL(member.User, cdnAvatar)
					if err != nil {
						return fmt.Errorf("error downloading avatar for user %s: %v", member.User.ID, err)
					}
				}

				p.cache.WriteNew(p.log, logentry.Make("history", "del", member))
				p.ever["member"][msgs[i].Author.ID] = true
			}

			for _, u := range msgs[i].Mentions {
				if !p.ever["member"][u.ID] {
					member := &discordgo.Member{User: u}

					if member.User.Avatar != "" {
						err := p.cdnDL(member.User, cdnAvatar)
						if err != nil {
							return fmt.Errorf("error downloading avatar for user %s: %v", member.User.ID, err)
						}
					}

					p.cache.WriteNew(p.log, logentry.Make("history", "del", member))
					p.ever["member"][u.ID] = true
				}
			}

			for _, match := range regexp.MustCompile("<(a?):[^:]+:([0-9]+)>").FindAllStringSubmatch(msgs[i].Content, -1) {
				e := &discordgo.Emoji{ID: match[2], Animated: match[1] == "a"}
				err := p.cdnDL(e, 0)
				if err != nil {
					return fmt.Errorf("error downloading external emoji %s: %v", e.ID, err)
				}
			}

			for _, e := range msgs[i].Embeds {
				tsv.Write(f, logentry.Make("history", "add", &logentry.Embed{*e, msgs[i].ID}))
			}

			for _, a := range msgs[i].Attachments {
				err := p.cdnDL(a, 0)
				if err != nil {
					return fmt.Errorf("error downloading attachment %s: %v", a.ID, err)
				}
				tsv.Write(f, logentry.Make("history", "add", &logentry.Attachment{*a, msgs[i].ID}))
			}

			for _, r := range msgs[i].Reactions {
				if r.Emoji.ID != "" {
					err := p.cdnDL(r.Emoji, 0)
					if err != nil {
						return fmt.Errorf("error downloading external emoji %s: %v", r.Emoji.ID, err)
					}
				}

				// workaround until https://github.com/bwmarrin/discordgo/pull/615 is merged
				name := strings.Replace(r.Emoji.APIName(), "#", "%23", -1)
				users, err := p.d.MessageReactions(c.ID, msgs[i].ID, name, 100)
				if err != nil {
					return fmt.Errorf("error getting users for reaction %s to %s: %v", r.Emoji.APIName(), msgs[i].ID, err)
				}

				for _, u := range users {
					reaction := &logentry.Reaction{
						discordgo.MessageReaction{u.ID, msgs[i].ID, *r.Emoji, c.ID},
						1,
					}

					tsv.Write(f, logentry.Make("history", "add", reaction))
				}

				if r.Count > 100 {
					reaction := &logentry.Reaction{
						discordgo.MessageReaction{"", msgs[i].ID, *r.Emoji, c.ID},
						r.Count - 100,
					}
					tsv.Write(f, logentry.Make("history", "add", reaction))
				}
			}

			tsv.Write(f, logentry.Make("history", "add", msgs[i]))
		}

		log.Printf("[%s/%s] downloaded %d messages, last id %s with content %s", c.GuildID, c.ID, len(msgs), msgs[0].ID, msgs[0].Content)
	}

	p.log.Sync()
	return nil
}
