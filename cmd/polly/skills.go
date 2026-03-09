package main

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

// skillCatalogResult holds the catalog and metadata for --skill flag handling.
type skillCatalogResult struct {
	catalog      *skills.Catalog
	autoActivate []string // skill names to auto-activate from --skill
	sources      []string // original --skill sources for persistence
}

// resolveSkillSources resolves --skill sources into directories and skill names.
func resolveSkillSources(sources []string) (dirs, names []string, err error) {
	for _, source := range sources {
		resolved, resolveErr := skills.ResolveSkill(source)
		if resolveErr != nil {
			return nil, nil, fmt.Errorf("resolve skill %s: %w", source, resolveErr)
		}
		dirs = append(dirs, resolved.Dir)
		names = append(names, resolved.Name)
	}
	return dirs, names, nil
}

func loadSkillCatalog(config *Config, persistedSources []string) (*skillCatalogResult, error) {
	if config.NoSkills {
		return &skillCatalogResult{}, nil
	}

	// --skill on command line takes precedence over persisted sources
	sources := config.Skills
	if len(sources) == 0 {
		sources = persistedSources
	}

	dirs := append([]string{}, config.SkillDirs...)
	skillDirs, autoActivate, err := resolveSkillSources(sources)
	if err != nil {
		return nil, err
	}
	dirs = append(dirs, skillDirs...)

	catalog, err := skills.LoadCatalog(dirs)
	if err != nil {
		return nil, err
	}

	return &skillCatalogResult{
		catalog:      catalog,
		autoActivate: autoActivate,
		sources:      sources,
	}, nil
}

func newSkillRuntime(catalog *skills.Catalog, registry *tools.ToolRegistry) (*tools.SkillRuntime, error) {
	if registry == nil {
		return nil, nil
	}
	return tools.NewSkillRuntime(catalog, registry)
}

func restoreActiveSkills(metadata *sessions.Metadata, skillRuntime *tools.SkillRuntime) error {
	if metadata == nil || skillRuntime == nil || len(metadata.ActiveSkills) == 0 {
		return nil
	}
	return skillRuntime.Restore(metadata.ActiveSkills)
}

func autoActivateSkills(names []string, skillRuntime *tools.SkillRuntime) error {
	if skillRuntime == nil || len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if _, err := skillRuntime.Activate(name); err != nil {
			return fmt.Errorf("auto-activate skill %s: %w", name, err)
		}
	}
	return nil
}

func persistActiveSkills(session sessions.Session, skillRuntime *tools.SkillRuntime, skillSources []string) error {
	if session == nil || skillRuntime == nil {
		return nil
	}

	activeSkills := skillRuntime.ActivatedSkills()
	if len(activeSkills) == 0 && len(skillSources) == 0 {
		return nil
	}

	return session.UpdateMetadata(&sessions.Metadata{
		ActiveSkills: activeSkills,
		SkillSources: skillSources,
	})
}

func handleListSkills(config *Config) error {
	if config.NoSkills {
		fmt.Println("Skills are disabled")
		return nil
	}

	result, err := loadSkillCatalog(config, nil)
	if err != nil {
		return err
	}

	if result.catalog == nil || result.catalog.IsEmpty() {
		fmt.Println("No skills found")
		return nil
	}

	for _, skill := range result.catalog.List() {
		fmt.Printf("%s - %s [%s]\n", skill.Name, skill.Description, skill.RootDir)
	}
	return nil
}
