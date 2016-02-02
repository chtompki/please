// Diretory-based cache.

package cache

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path"

	"core"
)

type dirCache struct {
	Dir string
}

func (cache *dirCache) Store(target *core.BuildTarget, key []byte) {
	cacheDir := cache.getPath(target, key)
	// Clear out anything that might already be there.
	if err := os.RemoveAll(cacheDir); err != nil {
		log.Warning("Failed to remove existing cache directory %s: %s", cacheDir, err)
		return
	}
	for out := range cacheArtifacts(target) {
		cache.StoreExtra(target, key, out)
	}
}

func (cache *dirCache) StoreExtra(target *core.BuildTarget, key []byte, out string) {
	cacheDir := cache.getPath(target, key)
	log.Debug("Storing %s: %s in dir cache...", target.Label, out)
	if dir := path.Dir(out); dir != "." {
		if err := os.MkdirAll(path.Join(cacheDir, dir), core.DirPermissions); err != nil {
			log.Warning("Failed to create cache directory %s: %s", path.Join(cacheDir, dir), err)
			return
		}
	}
	outFile := path.Join(core.RepoRoot, target.OutDir(), out)
	cachedFile := path.Join(cacheDir, out)
	// Remove anything existing
	if err := os.RemoveAll(cachedFile); err != nil {
		log.Warning("Failed to remove existing cached file %s: %s", cachedFile, err)
	} else if err := os.MkdirAll(cacheDir, core.DirPermissions); err != nil {
		log.Warning("Failed to create cache directory %s: %s", cacheDir, err)
		return
	} else if err := core.RecursiveCopyFile(outFile, cachedFile, fileMode(target), false); err != nil {
		// Cannot hardlink files into the cache, must copy them for reals.
		log.Warning("Failed to store cache file %s: %s", cachedFile, err)
	}
}

func (cache *dirCache) Retrieve(target *core.BuildTarget, key []byte) bool {
	cacheDir := cache.getPath(target, key)
	if !core.PathExists(cacheDir) {
		log.Debug("%s: %s doesn't exist in dir cache", target.Label, cacheDir)
		return false
	}
	for out := range cacheArtifacts(target) {
		if !cache.RetrieveExtra(target, key, out) {
			return false
		}
	}
	return true
}

func (cache *dirCache) RetrieveExtra(target *core.BuildTarget, key []byte, out string) bool {
	outDir := path.Join(core.RepoRoot, target.OutDir())
	cacheDir := cache.getPath(target, key)
	cachedOut := path.Join(cacheDir, out)
	realOut := path.Join(outDir, out)
	if !core.PathExists(cachedOut) {
		log.Debug("%s: %s doesn't exist in dir cache", target.Label, cachedOut)
		return false
	}
	log.Debug("Retrieving %s: %s from dir cache...", target.Label, cachedOut)
	if dir := path.Dir(realOut); dir != "." {
		if err := os.MkdirAll(dir, core.DirPermissions); err != nil {
			log.Warning("Failed to create output directory %s: %s", dir, err)
			return false
		}
	}
	// It seems to be quite important that we unlink the existing file first to avoid ETXTBSY errors
	// in cases where we're running an existing binary (as Please does during bootstrap, for example).
	if err := os.RemoveAll(realOut); err != nil {
		log.Warning("Failed to unlink existing output %s: %s", realOut, err)
		return false
	}
	// Recursively hardlink files back out of the cache
	if err := core.RecursiveCopyFile(cachedOut, realOut, fileMode(target), true); err != nil {
		log.Warning("Failed to move cached file to output: %s -> %s: %s", cachedOut, realOut, err)
		return false
	}
	log.Debug("Retrieved %s: %s from dir cache", target.Label, cachedOut)
	return true
}

func (cache *dirCache) Clean(target *core.BuildTarget) {
	// Remove for all possible keys, so can't get getPath here
	if err := os.RemoveAll(path.Join(cache.Dir, target.Label.PackageName, target.Label.Name)); err != nil {
		log.Warning("Failed to remove artifacts for %s from dir cache: %s", target.Label, err)
	}
}

func (cache *dirCache) getPath(target *core.BuildTarget, key []byte) string {
	// NB. Is very important to use a padded encoding here so lengths are consistent for cache_cleaner.
	return path.Join(cache.Dir, target.Label.PackageName, target.Label.Name, base64.URLEncoding.EncodeToString(core.CollapseHash(key)))
}

func newDirCache(config core.Configuration) *dirCache {
	cache := new(dirCache)
	// Absolute paths are allowed. Relative paths are interpreted relative to the repo root.
	if config.Cache.Dir[0] == '/' {
		cache.Dir = config.Cache.Dir
	} else {
		cache.Dir = path.Join(core.RepoRoot, config.Cache.Dir)
	}
	// Make directory if it doesn't exist.
	if err := os.MkdirAll(cache.Dir, core.DirPermissions); err != nil {
		panic(fmt.Sprintf("Failed to create root cache directory %s: %s", cache.Dir, err))
	}
	// Fire off the cache cleaner process.
	if config.Cache.DirCacheCleaner != "" {
		go func() {
			log.Info("Running cache cleaner: %s --dir %s --high_water_mark %s --low_water_mark %s",
				config.Cache.DirCacheCleaner, cache.Dir, config.Cache.DirCacheHighWaterMark, config.Cache.DirCacheLowWaterMark)
			cmd := exec.Command(config.Cache.DirCacheCleaner,
				"--dir", cache.Dir,
				"--high_water_mark", config.Cache.DirCacheHighWaterMark,
				"--low_water_mark", config.Cache.DirCacheLowWaterMark)
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Error("Cache cleaner error %s.\nFull output: %s", err, out)
			}
		}()
	}
	return cache
}

func fileMode(target *core.BuildTarget) os.FileMode {
	if target.IsBinary {
		return 0555
	} else {
		return 0444
	}
}