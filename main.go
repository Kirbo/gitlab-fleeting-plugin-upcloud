package main

import "gitlab.com/gitlab-org/fleeting/fleeting/plugin"

func main() {
	plugin.Main(&InstanceGroup{}, Version)
}
