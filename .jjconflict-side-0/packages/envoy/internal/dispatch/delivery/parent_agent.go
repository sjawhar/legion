package delivery

import "strings"

// ParentAgents resolves a pull request's first session to its Parent agent facet value
// (LEGION-682): the session group holding it, else the session shown by its latest title, else by
// its bare id. A session outside every group whose title is a group's name, ignoring case, carries
// its id beside the title, so the group and that session never share a label: the group-name rule
// (groups.go refuseTitleClash) keeps that from happening when a group is named, and this covers a
// session that takes a group's name as its title afterwards.
type ParentAgents struct {
	groupOf map[string]SessionGroup
	titles  map[string]string
	names   map[string]bool
}

// NewParentAgents builds the resolver from every session group and the sessions' titles, as
// ResolveSessionTitles answers them.
func NewParentAgents(groups []SessionGroup, titles map[string]string) ParentAgents {
	agents := ParentAgents{
		groupOf: make(map[string]SessionGroup),
		titles:  titles,
		names:   make(map[string]bool, len(groups)),
	}
	for _, group := range groups {
		agents.names[strings.ToLower(group.Name)] = true
		for _, session := range group.Sessions {
			agents.groupOf[session] = group
		}
	}
	return agents
}

// Value is the Parent agent facet value of a pull request whose first session is session.
func (p ParentAgents) Value(session string) string {
	if group, ok := p.groupOf[session]; ok {
		return group.ParentAgentValue()
	}
	title := SessionTitle(p.titles, session)
	if _, hasTitle := p.titles[session]; hasTitle && p.names[strings.ToLower(title)] {
		return title + " (" + session + ")"
	}
	return title
}
