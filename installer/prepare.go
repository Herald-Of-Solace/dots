package installer

import (
	"os"
	"sync"

	"go.evanpurkhiser.com/dots/config"
	"go.evanpurkhiser.com/dots/resolver"
)

// PreparedInstall represents the set of dotfiles that which have been prepared
// for installation, and the install scripts which are associated to the set of
// dotfiles. The install scripts from dotfiles have been normalized so that
// each script is only represented once in the list.
type PreparedInstall struct {
	Dotfiles       []*PreparedDotfile
	InstallScripts []*InstallScript
}

// A PreparedDotfile represents a dotfile that has been "prepared" for
// installation by verifying it's contents against the existing dotfile, and
// checking various other flags that require knowledge of the existing dotfile.
type PreparedDotfile struct {
	*resolver.Dotfile

	// IsNew indicates that the dotfile does not currently exist. A dotfile can
	// be `Added` and not `IsNew` if a previous untracked dotfile file exists.
	IsNew bool

	// ContentsDiffer is a boolean flag representing that the compiled source
	// differs from the currently installed dotfile.
	ContentsDiffer bool

	// SourcesAreIrregular indicates that a compiled dotfile (one with multiple
	// sources) does not have all regular file sources. This dotfile cannot be
	// compiled or installed.
	SourcesAreIrregular bool

	// Mode represents the change in the file mode bits of the source and
	// target os.FileMode (does not include permissions). This will not be set
	// if the sources are irregular.
	Mode *FileMode

	// Permissions represents the change in permission between the compiled source
	// and the currently installed dotfile. Equal permissions can be verified
	// by calling Permissions.IsSame.
	Permissions *FileMode

	// SourcePermissionsDiffer indicates that a compiled dotfile (one with
	// multiple sources) does not have consistent permissions across all
	// sources. In this case the lowest mode will be used.
	SourcePermissionsDiffer bool

	// RemovedNull is a warning flag indicating that the removed dotfile does
	// not exist in the install tree, though the dotfile is marked as removed.
	RemovedNull bool

	// OverwritesExisting is a warning flag that indicates that installing this
	// dotfile is overwriting a dotfile that was not part of the lockfile.
	OverwritesExisting bool

	// PrepareError keeps track of errors while preparing the dotfile. Should
	// this contain any errors, the PreparedDotfile is likely incomplete.
	PrepareError error

	sourceInfo []os.FileInfo
}

// IsChanged reports if the prepared dotfile has changes from the target
// dotfile.
func (p *PreparedDotfile) IsChanged() bool {
	return p.IsNew || p.ContentsDiffer || p.Permissions.IsChanged() || p.Mode.IsChanged()
}

// FileMode represents the new and old dotfile file mode.
type FileMode struct {
	Old os.FileMode
	New os.FileMode
}

// IsChanged returns a boolean value indicating if the modes are equal.
func (d FileMode) IsChanged() bool {
	return d.New != d.Old
}

// InstallScript represents a single installation script that is mapped to one
// or more dotfiles.
type InstallScript struct {
	Path       string
	RequiredBy []*PreparedDotfile
}

// ShouldInstall indicates weather the installation script should be executed.
// This will check weather any of the required dotfiles have changed.
func (i *InstallScript) ShouldInstall() bool {
	for _, dotfile := range i.RequiredBy {
		if dotfile.IsChanged() {
			return true
		}
	}

	return false
}

// PrepareDotfiles iterates all passed dotfiles and creates an associated
// PreparedDotfile, returning a PreparedInstall object.
func PrepareDotfiles(dotfiles resolver.Dotfiles, config config.SourceConfig) PreparedInstall {
	preparedDotfiles := make([]*PreparedDotfile, len(dotfiles))

	waitGroup := sync.WaitGroup{}
	waitGroup.Add(len(dotfiles))

	prepare := func(index int, dotfile *resolver.Dotfile) {
		defer waitGroup.Done()

		installPath := config.InstallPath + separator + dotfile.Path

		prepared := PreparedDotfile{
			Dotfile: dotfile,
		}
		preparedDotfiles[index] = &prepared

		targetInfo, targetStatErr := os.Lstat(installPath)

		exists := !os.IsNotExist(targetStatErr)
		prepared.IsNew = exists

		// If we're unable to stat our target installation file and the file
		// exists there's likely a permissions issue.
		if targetStatErr != nil && exists {
			prepared.PrepareError = targetStatErr
			return
		}

		// Nothing needs to be verified if the dotfile is simply being added
		if dotfile.Added && !exists {
			return
		}

		if dotfile.Added && exists {
			prepared.OverwritesExisting = true
		}

		if dotfile.Removed && !exists {
			prepared.RemovedNull = true
		}

		sourceInfo := make([]os.FileInfo, len(dotfile.Sources))

		prepared.sourceInfo = sourceInfo

		for i, source := range dotfile.Sources {
			path := config.SourcePath + separator + source.Path

			info, err := os.Lstat(path)
			if err != nil {
				prepared.PrepareError = err
				return
			}
			sourceInfo[i] = info
		}

		sourcePermissions, tookLowest := flattenPermissions(sourceInfo)

		prepared.Permissions = &FileMode{
			Old: targetInfo.Mode() & os.ModePerm,
			New: sourcePermissions,
		}
		prepared.SourcePermissionsDiffer = tookLowest

		prepared.SourcesAreIrregular = !isAllRegular(sourceInfo)

		if !prepared.SourcesAreIrregular {
			prepared.Mode = &FileMode{
				Old: targetInfo.Mode() &^ os.ModePerm,
				New: sourceInfo[0].Mode() &^ os.ModePerm,
			}
		}

		// If the dotfile does not require compilation we can directly compare
		// the size of the (single) source file with the current target source.
		// Otherwise we will have to compile to compare.
		if !shouldCompile(dotfile, config) && targetInfo.Size() != sourceInfo[0].Size() {
			prepared.ContentsDiffer = true
			return
		}

		source, err := OpenDotfile(dotfile, config)
		if err != nil {
			prepared.PrepareError = err
			return
		}
		defer source.Close()

		target, err := os.Open(installPath)
		if err != nil {
			prepared.PrepareError = err
			return
		}
		defer target.Close()

		filesAreSame, err := compareReaders(source, target)
		if err != nil {
			prepared.PrepareError = err
		}

		prepared.ContentsDiffer = !filesAreSame
	}

	for i, dotfile := range dotfiles {
		go prepare(i, dotfile)
	}

	waitGroup.Wait()

	// Once all dotfiles have been prepared, we can prepare the list of
	// InstallScripts. This list will be normalized sot that each install script
	// appears only once.
	scriptMap := map[string][]*PreparedDotfile{}

	for _, dotfile := range preparedDotfiles {
		for _, path := range dotfile.InstallScripts {
			if scriptMap[path] == nil {
				scriptMap[path] = []*PreparedDotfile{dotfile}
			} else {
				scriptMap[path] = append(scriptMap[path], dotfile)
			}
		}
	}

	installScripts := make([]*InstallScript, 0, len(scriptMap))

	for path, dotfiles := range scriptMap {
		installScripts = append(installScripts, &InstallScript{
			RequiredBy: dotfiles,
			Path:       path,
		})
	}

	return PreparedInstall{
		Dotfiles:       preparedDotfiles,
		InstallScripts: installScripts,
	}
}
