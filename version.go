package main

import "gitlab.com/gitlab-org/fleeting/fleeting/plugin"

var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

var Version = plugin.VersionInfo{
	Name:      "fleeting-plugin-upcloud",
	Version:   buildVersion,
	Revision:  buildCommit,
	Reference: "gitlab.com/kirbo/fleeting-plugin-upcloud",
	BuiltAt:   buildDate,
}
