package inc_files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"nxs-backup/modules/backend/exec_cmd"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/mb0/glob"

	"nxs-backup/interfaces"
	"nxs-backup/misc"
	"nxs-backup/modules/backend/targz"
	"nxs-backup/modules/logger"
)

type job struct {
	name            string
	tmpDir          string
	safetyBackup    bool
	deferredCopying bool
	storages        interfaces.Storages
	targets         map[string]target
	dumpedObjects   map[string]interfaces.DumpObject
}

type target struct {
	path        string
	gzip        bool
	saveAbsPath bool
	excludes    []string
}

type JobParams struct {
	Name            string
	TmpDir          string
	SafetyBackup    bool
	DeferredCopying bool
	Storages        interfaces.Storages
	Sources         []SourceParams
}

type SourceParams struct {
	Name        string
	Targets     []string
	Excludes    []string
	Gzip        bool
	SaveAbsPath bool
}

func Init(jp JobParams) (interfaces.Job, error) {
	// check if tar and gzip available
	if _, err := exec_cmd.Exec("tar", "--version"); err != nil {
		return nil, fmt.Errorf("Job `%s` init failed. Can't check `tar` version. Please install `tar`. Error: %s ", jp.Name, err)
	}

	j := &job{
		name:            jp.Name,
		tmpDir:          jp.TmpDir,
		safetyBackup:    jp.SafetyBackup,
		deferredCopying: jp.DeferredCopying,
		storages:        jp.Storages,
		dumpedObjects:   make(map[string]interfaces.DumpObject),
		targets:         make(map[string]target),
	}

	for _, src := range jp.Sources {

		for _, targetPattern := range src.Targets {

			for strings.HasSuffix(targetPattern, "/") {
				targetPattern = strings.TrimSuffix(targetPattern, "/")
			}

			targetOfsList, err := filepath.Glob(targetPattern)
			if err != nil {
				return nil, fmt.Errorf("Job `%s` init failed. Unable to process pattern: %s. Error: %s. ", jp.Name, targetPattern, err)
			}

			for _, ofs := range targetOfsList {
				var excludes []string

				skipOfs := false
				for _, pattern := range src.Excludes {
					match, err := glob.Match(pattern, ofs)
					if err != nil {
						return nil, fmt.Errorf("Job `%s` init failed. Unable to process pattern: %s. Error: %s. ", jp.Name, pattern, err)
					}
					if match {
						skipOfs = true
					}

					excludes = append(excludes, pattern)
				}

				if !skipOfs {
					ofsPart := src.Name + "/" + misc.GetOfsPart(targetPattern, ofs)
					j.targets[ofsPart] = target{
						path:        ofs,
						gzip:        src.Gzip,
						saveAbsPath: src.SaveAbsPath,
						excludes:    excludes,
					}
				}
			}
		}
	}

	return j, nil
}

func (j *job) GetName() string {
	return j.name
}

func (j *job) GetTempDir() string {
	return j.tmpDir
}

func (j *job) GetType() string {
	return "inc_files"
}

func (j *job) GetTargetOfsList() (ofsList []string) {
	for ofs := range j.targets {
		ofsList = append(ofsList, ofs)
	}
	return
}

func (j *job) GetStoragesCount() int {
	return len(j.storages)
}

func (j *job) GetDumpObjects() map[string]interfaces.DumpObject {
	return j.dumpedObjects
}

func (j *job) SetDumpObjectDelivered(ofs string) {
	dumpObj := j.dumpedObjects[ofs]
	dumpObj.Delivered = true
	j.dumpedObjects[ofs] = dumpObj
}

func (j *job) IsBackupSafety() bool {
	return j.safetyBackup
}

func (j *job) DeleteOldBackups(logCh chan logger.LogRecord, ofsPath string) error {
	return j.storages.DeleteOldBackups(logCh, j, ofsPath)
}

func (j *job) CleanupTmpData() error {
	return j.storages.CleanupTmpData(j)
}

func (j *job) NeedToMakeBackup() bool {
	return true
}

func (j *job) NeedToUpdateIncMeta() bool {
	return true
}

func (j *job) DoBackup(logCh chan logger.LogRecord, tmpDir string) error {
	var errs *multierror.Error

	for ofsPart, tgt := range j.targets {
		tmpBackupFile := misc.GetFileFullPath(tmpDir, ofsPart, "tar", "", tgt.gzip)
		err := os.MkdirAll(path.Dir(tmpBackupFile), os.ModePerm)
		if err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Unable to create tmp dir with next error: %s", err)
			errs = multierror.Append(errs, err)
			continue
		}

		initMeta, err := j.getPreviousMetadata(logCh, ofsPart, tmpBackupFile)
		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}

		if initMeta {
			logCh <- logger.Log(j.name, "").Info("Incremental backup will be reinitialized.")

			if err = j.DeleteOldBackups(logCh, ofsPart); err != nil {
				errs = multierror.Append(errs, err)
			}
			if _, err = os.Create(tmpBackupFile + ".init"); err != nil {
				errs = multierror.Append(errs, err)
			}
		}

		if err = targz.Tar(tgt.path, tmpBackupFile, true, tgt.gzip, tgt.saveAbsPath, tgt.excludes); err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Failed to create temp backup %s", tmpBackupFile)
			logCh <- logger.Log(j.name, "").Error(err)
			if serr, ok := err.(targz.Error); ok {
				logCh <- logger.Log(j.name, "").Debugf("STDERR: %s", serr.Stderr)
			}
			errs = multierror.Append(errs, err)
			continue
		}

		logCh <- logger.Log(j.name, "").Debugf("Created temp backup %s", tmpBackupFile)

		j.dumpedObjects[ofsPart] = interfaces.DumpObject{TmpFile: tmpBackupFile}
		if !j.deferredCopying {
			if err = j.storages.Delivery(logCh, j); err != nil {
				logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
				errs = multierror.Append(errs, err)
			}
		}
	}

	if err := j.storages.Delivery(logCh, j); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

func (j *job) getPreviousMetadata(logCh chan logger.LogRecord, ofsPart, tmpBackupFile string) (initMeta bool, err error) {
	var yearMetaFile, monthMetaFile, dayMetaFile io.Reader

	//year := misc.GetDateTimeNow("year")
	moy := misc.GetDateTimeNow("moy")
	dom := misc.GetDateTimeNow("dom")

	initMeta = misc.GetDateTimeNow("doy") == misc.YearlyBackupDay

	yearMetaFile, err = j.getMetadataFile(logCh, ofsPart, "year.inc")
	if err != nil {
		logCh <- logger.Log(j.name, "").Warnf("Failed to find backup year metadata. Error: %v", err)
		initMeta = true
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}

	if !initMeta {
		var dstMtdFile *os.File
		dstMtdFile, err = os.Create(tmpBackupFile + ".inc")
		if err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Failed to create new metadata file. Error: %v", err)
			return
		}

		if !misc.Contains(misc.DecadesBackupDays, dom) {
			dayMetaFile, err = j.getMetadataFile(logCh, ofsPart, "day.inc")
			if err != nil {
				logCh <- logger.Log(j.name, "").Error("Failed to find backup day metadata.")
				return
			} else {
				_, err = io.Copy(dstMtdFile, dayMetaFile)
				if err != nil {
					logCh <- logger.Log(j.name, "").Errorf("Failed to copy `day` metadata. Error: %v", err)
					return
				}
			}
		} else if moy != "1" {
			monthMetaFile, err = j.getMetadataFile(logCh, ofsPart, "month.inc")
			if err != nil {
				logCh <- logger.Log(j.name, "").Error("Failed to find backup month metadata.")
				return
			} else {
				_, err = io.Copy(dstMtdFile, monthMetaFile)
				if err != nil {
					logCh <- logger.Log(j.name, "").Errorf("Failed to copy `month` metadata. Error: %v", err)
					return
				}
			}
		} else {
			_, err = io.Copy(dstMtdFile, yearMetaFile)
			if err != nil {
				logCh <- logger.Log(j.name, "").Errorf("Failed to copy `year` metadata. Error: %v", err)
				return
			}
		}
	}
	return
}

// check and get metadata files (include remote storages)
func (j *job) getMetadataFile(logCh chan logger.LogRecord, ofsPart, metadata string) (reader io.Reader, err error) {
	year := misc.GetDateTimeNow("year")

	for i := len(j.storages) - 1; i >= 0; i-- {
		st := j.storages[i]

		reader, err = st.GetFileReader(path.Join(ofsPart, year, "inc_meta_info", metadata))
		if err != nil {
			logCh <- logger.Log(j.name, st.GetName()).Warnf("Unable to get previous metadata '%s' from storage. Error: %s ", metadata, err)
			continue
		}
		break
	}

	if reader == nil {
		err = fs.ErrNotExist
	}

	return
}

func (j *job) Close() error {
	for _, st := range j.storages {
		_ = st.Close()
	}
	return nil
}
