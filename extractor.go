package empire

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"

	"golang.org/x/net/context"

	"github.com/remind101/empire/pkg/image"
	"github.com/remind101/empire/procfile"

	"github.com/fsouza/go-dockerclient"
)

var (
	// ProcfileName is the name of the Procfile file.
	ProcfileName = "Procfile"
)

// ProcfileExtractor represents something that can extract a Procfile from an image.
type ProcfileExtractor interface {
	Extract(context.Context, image.Image, io.Writer) ([]byte, error)
}

type ProcfileExtractorFunc func(context.Context, image.Image, io.Writer) ([]byte, error)

func (fn ProcfileExtractorFunc) Extract(ctx context.Context, image image.Image, w io.Writer) ([]byte, error) {
	return fn(ctx, image, w)
}

// CommandExtractor is an Extractor implementation that returns a Procfile based
// on the CMD directive in the Dockerfile. It makes the assumption that the cmd
// is a "web" process.
type CMDExtractor struct {
	// Client is the docker client to use to pull the container image.
	client *docker.Client
}

func NewCMDExtractor(c *docker.Client) *CMDExtractor {
	return &CMDExtractor{client: c}
}

func (e *CMDExtractor) Extract(_ context.Context, img image.Image, _ io.Writer) ([]byte, error) {
	i, err := e.client.InspectImage(img.String())
	if err != nil {
		return nil, err
	}

	return procfile.Marshal(procfile.ExtendedProcfile{
		"web": procfile.Process{
			Command: i.Config.Cmd,
		},
	})
}

// MultiExtractor is an Extractor implementation that tries multiple Extractors
// in succession until one succeeds.
func MultiExtractor(extractors ...ProcfileExtractor) ProcfileExtractor {
	return ProcfileExtractorFunc(func(ctx context.Context, image image.Image, w io.Writer) ([]byte, error) {
		for _, extractor := range extractors {
			p, err := extractor.Extract(ctx, image, w)

			// Yay!
			if err == nil {
				return p, nil
			}

			// Try the next one
			if _, ok := err.(*ProcfileError); ok {
				continue
			}

			// Bubble up the error
			return p, err
		}

		return nil, &ProcfileError{
			Err: errors.New("no suitable Procfile extractor found"),
		}
	})
}

// FileExtractor is an implementation of the Extractor interface that extracts
// the Procfile from the images WORKDIR.
type FileExtractor struct {
	// Client is the docker client to use to pull the container image.
	client *docker.Client
}

func NewFileExtractor(c *docker.Client) *FileExtractor {
	return &FileExtractor{client: c}
}

// Extract implements Extractor Extract.
func (e *FileExtractor) Extract(_ context.Context, img image.Image, w io.Writer) ([]byte, error) {
	c, err := e.createContainer(img)
	if err != nil {
		return nil, err
	}

	defer e.removeContainer(c.ID)

	pfile, err := e.procfile(c.ID)
	if err != nil {
		return nil, err
	}

	b, err := e.copyFile(c.ID, pfile)
	if err != nil {
		return nil, &ProcfileError{Err: err}
	}

	return b, nil
}

// procfile returns the path to the Procfile. If the container has a WORKDIR
// set, then this will return a path to the Procfile within that directory.
func (e *FileExtractor) procfile(id string) (string, error) {
	p := ""

	c, err := e.client.InspectContainer(id)
	if err != nil {
		return "", err
	}

	if c.Config != nil {
		p = c.Config.WorkingDir
	}

	return path.Join(p, ProcfileName), nil
}

// createContainer creates a new docker container for the given docker image.
func (e *FileExtractor) createContainer(img image.Image) (*docker.Container, error) {
	return e.client.CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image: img.String(),
		},
	})
}

// removeContainer removes a container by its ID.
func (e *FileExtractor) removeContainer(containerID string) error {
	return e.client.RemoveContainer(docker.RemoveContainerOptions{
		ID: containerID,
	})
}

// copyFile copies a file from a container.
func (e *FileExtractor) copyFile(containerID, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := e.client.CopyFromContainer(docker.CopyFromContainerOptions{
		Container:    containerID,
		Resource:     path,
		OutputStream: &buf,
	}); err != nil {
		return nil, err
	}

	// Open the tar archive for reading.
	r := bytes.NewReader(buf.Bytes())

	return firstFile(tar.NewReader(r))
}

// Example instance: Procfile doesn't exist
type ProcfileError struct {
	Err error
}

func (e *ProcfileError) Error() string {
	return fmt.Sprintf("Procfile not found: %s", e.Err)
}

// firstFile extracts the first file from a tar archive.
func firstFile(tr *tar.Reader) ([]byte, error) {
	if _, err := tr.Next(); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, tr); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func formationFromProcfile(app *App, p procfile.Procfile) (Formation, error) {
	switch p := p.(type) {
	case procfile.StandardProcfile:
		return formationFromStandardProcfile(app, p)
	case procfile.ExtendedProcfile:
		return formationFromExtendedProcfile(app, p)
	default:
		return nil, &ProcfileError{
			Err: errors.New("unknown Procfile format"),
		}
	}
}

func formationFromStandardProcfile(app *App, p procfile.StandardProcfile) (Formation, error) {
	f := make(Formation)

	for name, command := range p {
		cmd, err := ParseCommand(command)
		if err != nil {
			return nil, err
		}

		var exposure *Exposure
		if name == WebProcessType {
			exposure = defaultWebExposure(app)
		}

		f[name] = Process{
			Command: cmd,
			Expose:  exposure,
		}
	}

	return f, nil
}

func formationFromExtendedProcfile(app *App, p procfile.ExtendedProcfile) (Formation, error) {
	f := make(Formation)

	for name, process := range p {
		var (
			cmd      Command
			exposure *Exposure
			err      error
		)

		switch command := process.Command.(type) {
		case string:
			cmd, err = ParseCommand(command)
			if err != nil {
				return nil, err
			}
		case []interface{}:
			for _, v := range command {
				cmd = append(cmd, v.(string))
			}
		default:
			return nil, errors.New("unknown command format")
		}

		if e := process.Expose; e != nil {
			exposure = &Exposure{
				External: e.External,
				Protocol: e.Protocol,
				Cert:     app.Cert,
			}
		}

		f[name] = Process{
			Command: cmd,
			Expose:  exposure,
		}
	}

	return f, nil
}

// defaultWebExposure returns an *Exposure suitable for the default "web"
// process for standard Procfiles.
func defaultWebExposure(app *App) *Exposure {
	cert := app.Cert
	proto := "http"
	if cert != "" {
		proto = "https"
	}

	return &Exposure{
		External: app.Exposure == ExposePublic,
		Protocol: proto,
		Cert:     cert,
	}
}
