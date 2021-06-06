package info

import (
	"fmt"

	"github.com/jonas747/dcmd/v2"
	"github.com/jonas747/yagpdb/commands"
	"github.com/jonas747/yagpdb/common"
)

var Command = &commands.YAGCommand{
	CmdCategory: commands.CategoryGeneral,
	Name:        "Info",
	Description: "Responds with bot information",
	RunInDM:     true,
	RunFunc: func(data *dcmd.Data) (interface{}, error) {
		info := fmt.Sprintf(`**SaberBot - Helping Build A Better Discord!**
This bot focuses on being configurable and therefore is one of the more advanced bots.
It can perform a range of general purpose functionality (Reddit feeds, various commands, moderation utilities, automoderator functionality and so on) and it's configured through a web control panel.
I'm currently being run and developed by .🦈SHK8S#0001 (676195747904880690) but the bot is open source (<https://github.com/shk8s/saberbot>), so if you know Go and want to make some contributions, feel free to make a PR.
Control panel: <https://%s/manage>
				`, common.ConfHost.GetString())

		return info, nil
	},
}
