package up

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	dockerTypes "github.com/docker/docker/api/types"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/jbrekelmans/kube-compose/internal/pkg/docker"
	"github.com/jbrekelmans/kube-compose/internal/pkg/util"
	"github.com/pkg/errors"
)

var digestRegexp = docker.NewDigestRegexp()

func buildVolumeInitImageGetDockerfile(isDirSlice []bool) []byte {
	var b bytes.Buffer
	b.WriteString(`ARG BASE_IMAGE
FROM ${BASE_IMAGE}
`)
	for i := 1; i <= len(isDirSlice); i++ {
		if isDirSlice[i-1] {
			fmt.Fprintf(&b, "COPY data%d/ /app/data/vol%d/\n", i, i)
		} else {
			fmt.Fprintf(&b, "COPY data%d /app/data/vol%d\n", i, i)
		}
	}
	b.WriteString(`ENTRYPOINT ["bash", "-c", "`)
	for i := 1; i <= len(isDirSlice); i++ {
		if i > 1 {
			b.WriteString(" && ")
		}
		fmt.Fprintf(&b, "cp -r /app/data/vol%d /mnt/vol%d/root", i, i)
	}
	b.WriteString(`"]
`)
	return b.Bytes()
}

type bindMountHostFileToTarHelper struct {
	tw                     *tar.Writer
	twMayBeCorrupt         bool
	renameTo               string
	rootHostFile           string
	rootHostFileVol        string
	rootHostFileWithoutVol string
}

func (h *bindMountHostFileToTarHelper) runRegular(fileInfo os.FileInfo, hostFile, fileNameInTar string) error {
	header, err := tar.FileInfoHeader(fileInfo, "")
	if err != nil {
		return err
	}
	fd, err := os.Open(hostFile)
	if err != nil {
		return err
	}
	defer util.CloseAndLogError(fd)
	header.Name = fileNameInTar
	err = h.endHeaderCommon(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(h.tw, fd)
	if err != nil {
		h.twMayBeCorrupt = true
	}
	return err
}

func (h *bindMountHostFileToTarHelper) runDirectory(fileInfo os.FileInfo, hostFile, fileNameInTar string) error {
	fd, err := os.Open(hostFile)
	if err != nil {
		return err
	}
	defer util.CloseAndLogError(fd)
	header, err := tar.FileInfoHeader(fileInfo, "")
	if err != nil {
		return err
	}
	header.Name = fileNameInTar + "/"
	err = h.endHeaderCommon(header)
	if err != nil {
		return err
	}
	entries, err := fd.Readdir(0)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		err = h.runRecursive(
			entry,
			hostFile+string(filepath.Separator)+entry.Name(),
			header.Name+entry.Name(),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *bindMountHostFileToTarHelper) isFileWithinBindHostRoot(target string) bool {
	// Can assume target and h.rootHostFile are cleaned.
	// TODO https://github.com/jbrekelmans/kube-compose/issues/173 support case sensitive file systems
	vol := filepath.VolumeName(target)
	if vol != h.rootHostFileVol {
		return false
	}
	targetWithoutVol := target[len(vol):]
	if strings.HasPrefix(targetWithoutVol, h.rootHostFileWithoutVol) {
		if len(targetWithoutVol) == len(h.rootHostFileWithoutVol) {
			return true
		}
		if targetWithoutVol[len(h.rootHostFileWithoutVol)] == filepath.Separator {
			return true
		}
	}
	return false
}

func (h *bindMountHostFileToTarHelper) runSymlink(fileInfo os.FileInfo, hostFile, fileNameInTar string) error {
	// Symbolic link
	link, err := os.Readlink(hostFile)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("error while reading link %#v", hostFile))
	}
	var linkResolved string
	linkIsAbsLike := link != "" && (link[0] == '\\' || link[0] == '/')
	if linkIsAbsLike || filepath.VolumeName(link) != "" {
		// Windows:
		// Handle situations where the link is absolute, or relative to current working directory of a particular drive:
		// https://docs.microsoft.com/en-us/windows/desktop/api/winbase/nf-winbase-createsymboliclinka
		// This should be a noop on non-Windows because there will never be a non-empty volume and therefore the path must
		// be absolute.
		linkResolved, err = filepath.Abs(link)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("error while converting %#v to an absolute path", link))
		}
	} else {
		// Windows: no drive.
		// link is relative to parent directory.
		linkResolved = filepath.Join(filepath.Dir(hostFile), link)
	}
	// linkResolved will always be cleaned here, which is required for isFileWithinBindHostRoot.
	if h.isFileWithinBindHostRoot(linkResolved) {
		// Convert the target to an absolute path within the tar, normalising slashes.
		linkResolvedInTar := filepath.ToSlash(h.renameTo + linkResolved[len(h.rootHostFile):])
		// Convert the target to a relative path within the tar. This can be done a bit more efficiently since we know the paths are
		// relative, cleaned and slashed. We assign the error to underscore because it should never happen.
		linkResolvedInTarRel, _ := filepath.Rel(filepath.Dir(fileNameInTar), linkResolvedInTar)
		header, err := tar.FileInfoHeader(fileInfo, linkResolvedInTarRel)
		if err != nil {
			return err
		}
		header.Name = fileNameInTar
		return h.endHeaderCommon(header)
	}
	return fmt.Errorf("target of symlink %#v it outside the bind volume with host %#v", hostFile, h.rootHostFile)
}

func (h *bindMountHostFileToTarHelper) runRecursive(fileInfo os.FileInfo, hostFile, fileNameInTar string) error {
	switch {
	case (fileInfo.Mode() & os.ModeSymlink) != 0:
		// Symlink...
		return h.runSymlink(fileInfo, hostFile, fileNameInTar)
	case fileInfo.IsDir():
		// Directory...
		return h.runDirectory(fileInfo, hostFile, fileNameInTar)
	case fileInfo.Mode().IsRegular():
		// Regular file...
		return h.runRegular(fileInfo, hostFile, fileNameInTar)
	default:
		// The file is something else (e.g. a socket, a named pipe, a (char)device or an irregular file)
		return fmt.Errorf("file %#v is neither a symlink, a directory nor a regular file (os.ModeType 0x%x)",
			hostFile, fileInfo.Mode()&os.ModeType)
	}
}

func (h *bindMountHostFileToTarHelper) endHeaderCommon(header *tar.Header) error {
	// TODO https://github.com/jbrekelmans/kube-compose/issues/154 change owner of files here, as appropriate...
	// For example:
	// header.Uid = ...
	// header.Gid = ...
	err := h.tw.WriteHeader(header)
	if err != nil {
		h.twMayBeCorrupt = true
	}
	return err
}

func (h *bindMountHostFileToTarHelper) run(hostFile string, fileNameInTar string) (isDir bool, err error) {
	fileInfo, err := os.Lstat(hostFile)
	if err != nil {
		return
	}
	err = h.runRecursive(fileInfo, hostFile, fileNameInTar)
	if err == nil {
		isDir = fileInfo.IsDir()
		return
	}
	if h.twMayBeCorrupt {
		return
	}
	fmt.Printf("cannot simulate bind volume with host file %#v, interpreting as empty directory:", hostFile)
	fmt.Println(err)
	// Change type to an empty directory
	header, err := tar.FileInfoHeader(fileInfo, "")
	if err != nil {
		return
	}
	header.Typeflag = tar.TypeDir
	header.Name += "/"
	err = h.endHeaderCommon(header)
	isDir = true
	return
}

func bindMouseHostFileToTar(tw *tar.Writer, hostFile, renameTo string) (isDir bool, err error) {
	h := &bindMountHostFileToTarHelper{
		tw:           tw,
		rootHostFile: hostFile,
		renameTo:     renameTo,
	}
	vol := filepath.VolumeName(hostFile)
	h.rootHostFileVol = vol
	h.rootHostFileWithoutVol = hostFile[len(vol):]

	isDir, err = h.run(hostFile, renameTo)
	if err != nil {
		if h.twMayBeCorrupt {
			isDir = false
			return
		}
		fmt.Printf("cannot simulate bind volume with host file %#v, interpreting as empty directory:", hostFile)
		fmt.Println(err)
		header := &tar.Header{
			Name:     renameTo + "/",
			Typeflag: tar.TypeDir,
		}
		err = h.endHeaderCommon(header)
		isDir = true
	}
	return
}

func buildVolumeInitImageGetBuildContext(r *buildVolumeInitImageResult, bindVolumeHostPaths []string) ([]byte, error) {
	var tarBuffer bytes.Buffer
	tw := tar.NewWriter(&tarBuffer)
	defer tw.Close()

	for i, bindVolumeHostFile := range bindVolumeHostPaths {
		isDir, err := bindMouseHostFileToTar(tw, bindVolumeHostFile, fmt.Sprintf("data%d", i+1))
		if err != nil {
			return nil, err
		}
		r.isDirSlice = append(r.isDirSlice, isDir)
	}

	// Write Dockerfile to build context.
	dockerFile := buildVolumeInitImageGetDockerfile(r.isDirSlice)
	err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Size: int64(len(dockerFile)),
	})
	if err != nil {
		return nil, err
	}
	_, err = tw.Write(dockerFile)
	if err != nil {
		return nil, err
	}
	err = tw.Flush()
	if err != nil {
		return nil, err
	}
	return tarBuffer.Bytes(), nil
}

type buildVolumeInitImageResult struct {
	// Feedback whether or not each file is a directory, so that we can use subPath when creating Kubernetes volume mounts as appropriate.
	isDirSlice []bool
	imageID    string
}

func buildVolumeInitImage(ctx context.Context, dc *dockerClient.Client, bindVolumeHostPaths []string) (*buildVolumeInitImageResult, error) {
	r := &buildVolumeInitImageResult{}
	buildContextBytes, err := buildVolumeInitImageGetBuildContext(r, bindVolumeHostPaths)
	if err != nil {
		return nil, err
	}
	buildContext := bytes.NewReader(buildContextBytes)
	response, err := dc.ImageBuild(ctx, buildContext, dockerTypes.ImageBuildOptions{
		BuildArgs: map[string]*string{
			"BASE_IMAGE": util.NewString("ubuntu:latest"),
		},
		// Only the image ID is output when SupressOutput is true.
		SuppressOutput: true,
		Remove:         true,
	})
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var msg jsonmessage.JSONMessage
		err = decoder.Decode(&msg)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if loc := digestRegexp.FindStringIndex(msg.Stream); loc != nil {
			i := docker.Sha256BitLength/4 + len(docker.Sha256Prefix)
			r.imageID = msg.Stream[loc[0] : loc[0]+i]
		}
	}
	if r.imageID == "" {
		return nil, fmt.Errorf("could not parse image ID from docker build output stream")
	}
	return r, nil
}

func resolveBindVolumeHostPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	// Walk sections of path, evaluating symlinks in the process.
	vol := filepath.VolumeName(name)
	sep := string(filepath.Separator)
	parts := strings.Split(filepath.Clean(name[len(vol):]), sep)
	result := vol
	for i := 1; i < len(parts); i++ {
		result = result + sep + parts[i]
		resultResolved, err := filepath.EvalSymlinks(result)
		if os.IsNotExist(err) {
			if i+1 < len(parts) {
				result = result + sep + strings.Join(parts[i+1:], sep)
			}
			err = os.MkdirAll(result, 0777)
			return result, err
		}
		if err != nil {
			return "", err
		}
		result = resultResolved
	}
	return result, nil
}