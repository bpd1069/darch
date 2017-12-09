package images

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"../utils"
)

// ImageDefinition A struct representing an image to be built.
type ImageDefinition struct {
	Name      string
	ImageDir  string
	ImagesDir string
	Inherits  string
}

type imageConfiguration struct {
	Inherits string `json:"inherits"`
}

// BuildDefinition Parse an image from the file system
func BuildDefinition(imageName string, imagesDir string) (*ImageDefinition, error) {

	if len(imageName) == 0 {
		return nil, fmt.Errorf("An image must be provided")
	}

	if len(imagesDir) == 0 {
		return nil, fmt.Errorf("An image directory must be provided")
	}

	image := ImageDefinition{}

	image.ImagesDir = utils.ExpandPath(imagesDir)
	image.ImageDir = path.Join(image.ImagesDir, imageName)
	image.Name = imageName

	if !utils.DirectoryExists(image.ImageDir) {
		return nil, fmt.Errorf("Image directory %s doesn't exist", image.ImageDir)
	}

	imageConfiguration, err := loadImageConfiguration(image)

	if err != nil {
		return nil, err
	}

	image.Inherits = imageConfiguration.Inherits

	return &image, nil
}

func verifyDependencies(imageDefinition ImageDefinition, imageDefinitions map[string]ImageDefinition, currentStack map[string]bool) error {
	if currentStack == nil {
		currentStack = make(map[string]bool, 0)
	}

	if strings.HasPrefix(imageDefinition.Inherits, "external:") {
		// we reached the end, all good!
		return nil
	}

	if _, ok := currentStack[imageDefinition.Inherits]; ok {
		// Cyclical dependency detected!
		return fmt.Errorf("Image %s has a cyclical dependency", imageDefinition.Name)
	}

	// Make this image as having been traversed.
	currentStack[imageDefinition.Name] = true

	if parent, ok := imageDefinitions[imageDefinition.Inherits]; ok {
		return verifyDependencies(parent, imageDefinitions, currentStack)
	}

	return fmt.Errorf("Image defintion %s inherits from %s, which doesn't exist", imageDefinition.Name, imageDefinition.Inherits)
}

// BuildAllDefinitions Return all the images in the image directory
func BuildAllDefinitions(imagesDir string) (map[string]ImageDefinition, error) {
	if len(imagesDir) == 0 {
		return nil, fmt.Errorf("An image directory must be provided")
	}

	imageNames, err := utils.GetChildDirectories(imagesDir)

	if err != nil {
		return nil, err
	}

	imageDefinitions := make(map[string]ImageDefinition, 0)

	for _, imageName := range imageNames {
		imageDefinition, err := BuildDefinition(imageName, imagesDir)
		if err != nil {
			return nil, err
		}
		imageDefinitions[imageName] = *imageDefinition
	}

	// verify dependencies are satisfied and no circular dependencies
	for _, imageDefinition := range imageDefinitions {
		err := verifyDependencies(imageDefinition, imageDefinitions, nil)
		if err != nil {
			return nil, err
		}
	}

	return imageDefinitions, nil
}

// BuildImageLayer Run installation scripts on top of another image.
func BuildImageLayer(imageDefinition *ImageDefinition, tags []string, buildPrefix string, packageCache string, environmentVariables map[string]string) error {

	if len(packageCache) > 0 {
		if !utils.DirectoryExists(packageCache) {
			err := os.MkdirAll(packageCache, os.ModePerm)
			if err != nil {
				return err
			}
		}
	}

	inherits := imageDefinition.Inherits
	if strings.HasPrefix(inherits, "external:") {
		inherits = inherits[len("external:"):len(inherits)]
	} else {
		inherits = buildPrefix + inherits
	}

	log.Println("Building image " + buildPrefix + imageDefinition.Name + ".")
	log.Println("Using parent image " + inherits + ".")

	tmpImageName := "darch-building-" + imageDefinition.Name

	arguements := make([]string, 0)
	arguements = append(arguements, "run")
	arguements = append(arguements, "-d")
	arguements = append(arguements, "-v")
	arguements = append(arguements, imageDefinition.ImagesDir+":/images")
	if len(packageCache) > 0 {
		arguements = append(arguements, "-v")
		arguements = append(arguements, packageCache+":/packages")
	}
	arguements = append(arguements, "--privileged")
	arguements = append(arguements, "--name")
	arguements = append(arguements, tmpImageName)
	arguements = append(arguements, inherits)
	err := runCommand("docker", arguements...)
	if err != nil {
		return err
	}
	// Now that we have the container running withour mounts, let's let arch-chroot
	// know about them so they show up when the container does a chroot into the
	// rootfs (/root.x86_64).
	err = runCommand("docker", "exec", "--privileged", tmpImageName, "mkdir", "/root.x86_64/images/")
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}
	err = runCommand("docker", "exec", "--privileged", tmpImageName, "bash", "-c", "echo \"chroot_add_mount /images \\\"/root.x86_64/images\\\" --bind\" >> /arch-chroot-customizations")
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}
	if len(packageCache) > 0 {
		err = runCommand("docker", "exec", "--privileged", tmpImageName, "mkdir", "-p", "/root.x86_64/var/cache/pacman/pkg/")
		if err != nil {
			destroyContainer(tmpImageName)
			return err
		}
		err = runCommand("docker", "exec", "--privileged", tmpImageName, "bash", "-c", "echo \"chroot_add_mount /packages \\\"/root.x86_64/var/cache/pacman/pkg/\\\" --bind\" >> /arch-chroot-customizations")
		if err != nil {
			destroyContainer(tmpImageName)
			return err
		}
	}
	arguements = make([]string, 0)
	arguements = append(arguements, "exec")
	arguements = append(arguements, "--privileged")
	for environmentVariableName, environmentVariableValue := range environmentVariables {
		arguements = append(arguements, "-e")
		arguements = append(arguements, environmentVariableName+"="+environmentVariableValue)
	}
	arguements = append(arguements, tmpImageName)
	arguements = append(arguements, "arch-chroot-custom")
	arguements = append(arguements, "/root.x86_64")
	arguements = append(arguements, "/bin/bash")
	arguements = append(arguements, "-c")
	arguements = append(arguements, "cd /images/"+imageDefinition.Name+" && ./script")
	err = runCommand("docker", arguements...)
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}
	err = runCommand("docker", "exec", "--privileged", tmpImageName, "rm", "-r", "/root.x86_64/images")
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}
	err = runCommand("docker", "exec", "--privileged", tmpImageName, "rm", "-r", "-f", "/root.x86_64/var/cache/pacman/pkg/")
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	err = runCommand("docker", "commit", tmpImageName, buildPrefix+imageDefinition.Name)
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	for _, tag := range tags {
		err = runCommand("docker", "tag", imageDefinition.Name, buildPrefix+imageDefinition.Name+":"+tag)
		if err != nil {
			destroyContainer(tmpImageName)
			return err
		}
	}

	return destroyContainer(tmpImageName)
}

// ExtractImage Extracts an image (with tag) to a specified directory
func ExtractImage(name string, tag string, destination string) error {
	tmpImageName := "darch-extracting-" + strings.Replace(name, "/", "", -1)

	imageName := name
	if len(tag) > 0 {
		imageName = imageName + ":" + tag
	}

	if !utils.DirectoryExists(destination) {
		err := os.MkdirAll(destination, os.ModePerm)
		if err != nil {
			return err
		}
	}

	err := utils.CleanDirectory(destination)
	if err != nil {
		return err
	}

	err = runCommand("docker", "run", "-d", "--privileged", "--name", tmpImageName, imageName)
	if err != nil {
		return err
	}

	err = runCommand("docker", "exec", tmpImageName, "mksquashfs", "root.x86_64", "/rootfs.squash")
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	err = runCommand("docker", "cp", tmpImageName+":/rootfs.squash", path.Join(destination, "rootfs.squash"))
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	err = runCommand("docker", "cp", tmpImageName+":/root.x86_64/boot/vmlinuz-linux", path.Join(destination, "vmlinuz-linux"))
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	err = runCommand("docker", "cp", tmpImageName+":/root.x86_64/boot/initramfs-linux.img", path.Join(destination, "initramfs-linux.img"))
	if err != nil {
		destroyContainer(tmpImageName)
		return err
	}

	return destroyContainer(tmpImageName)
}

func loadImageConfiguration(image ImageDefinition) (*imageConfiguration, error) {
	imageConfigurationPath := path.Join(image.ImageDir, "config.json")
	imageConfiguration := imageConfiguration{}

	if !utils.FileExists(imageConfigurationPath) {
		return nil, fmt.Errorf("No configuration file exists at %s", imageConfigurationPath)
	}

	jsonData, err := ioutil.ReadFile(imageConfigurationPath)

	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(jsonData, &imageConfiguration)

	if err != nil {
		return nil, err
	}

	if len(imageConfiguration.Inherits) == 0 {
		return nil, fmt.Errorf("No inherit property given for image %s", image.Name)
	}

	return &imageConfiguration, nil
}

func runCommand(program string, args ...string) error {
	cmd := exec.Command(program, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

func destroyContainer(containerName string) error {
	err := runCommand("docker", "stop", containerName)
	if err != nil {
		return err
	}

	err = runCommand("docker", "rm", containerName)
	if err != nil {
		return err
	}

	return nil
}
