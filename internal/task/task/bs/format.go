/*
 *  Copyright (c) 2021 NetEase Inc.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

/*
 * Project: CurveAdm
 * Created Date: 2021-12-27
 * Author: Jingli Chen (Wine93)
 */

package bs

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/opencurve/curveadm/cli/cli"
	"github.com/opencurve/curveadm/internal/configure"
	os "github.com/opencurve/curveadm/internal/configure/os"
	"github.com/opencurve/curveadm/internal/configure/topology"
	"github.com/opencurve/curveadm/internal/errno"
	"github.com/opencurve/curveadm/internal/task/context"
	"github.com/opencurve/curveadm/internal/task/scripts"
	"github.com/opencurve/curveadm/internal/task/step"
	"github.com/opencurve/curveadm/internal/task/task"
	"github.com/opencurve/curveadm/internal/utils"
)

const (
	DEFAULT_CHUNKFILE_SIZE        = 16 * 1024 * 1024 // 16MB
	DEFAULT_CHUNKFILE_HEADER_SIZE = 4 * 1024         // 4KB

	WARNING_EDIT = "GENERATED BY CURVEADM, DONT EDIT THIS"

	SIGNATURE_NOT_A_BLOCK_DEVICE = "not a block device"

	// 82511eb8-e4e3-4a50-a736-d584fbf533fa
	REEGX_DEVICE_UUID = "^.{8}-.{4}-.{4}-.{4}-.{12}$"
)

type (
	step2EditFSTab struct {
		host       string
		device     string
		mountPoint string
		oldUUID    *string
		uuid       string
		skipAdd    bool
		curveadm   *cli.CurveAdm
	}
)

func skipFormat(containerId *string) step.LambdaType {
	return func(ctx *context.Context) error {
		if len(*containerId) > 0 {
			return task.ERR_SKIP_TASK
		}
		return nil
	}
}

func checkDeviceUUID(host, device string, success *bool, uuid *string) step.LambdaType {
	return func(ctx *context.Context) error {
		if !*success {
			if strings.Contains(*uuid, SIGNATURE_NOT_A_BLOCK_DEVICE) {
				return errno.ERR_NOT_A_BLOCK_DEVICE.
					F("host=%s device=%s", host, device)
			}
			return errno.ERR_GET_DEVICE_UUID_FAILED.
				F("host=%s device=%s uuid=%s", host, device, *uuid)
		}

		pattern := regexp.MustCompile(REEGX_DEVICE_UUID)
		mu := pattern.FindStringSubmatch(*uuid)
		if len(mu) == 0 {
			return errno.ERR_GET_DEVICE_UUID_FAILED.
				F("host=%s device=%s uuid=%s", host, device, *uuid)
		}
		return nil
	}
}

func (s *step2EditFSTab) expression(express2del, express2add *string) step.LambdaType {
	return func(ctx *context.Context) error {
		*express2del = fmt.Sprintf("/UUID=%s/d", *s.oldUUID)
		*express2add = fmt.Sprintf("$ a UUID=%s  %s  ext4  rw,errors=remount-ro  0  0  # %s",
			s.uuid, s.mountPoint, WARNING_EDIT)
		return nil
	}
}

func (s *step2EditFSTab) execute(ctx *context.Context) error {
	var express2del, express2add string
	curveadm := s.curveadm
	now := time.Now().Format("2006-01-02")
	steps := []task.Step{}

	var success bool
	steps = append(steps, &step.CopyFile{ // backup fstab
		Source:      os.GetFSTabPath(),
		Dest:        fmt.Sprintf("%s-%s.backup", os.GetFSTabPath(), now),
		NoClobber:   true,
		ExecOptions: curveadm.ExecOptions(),
	})
	steps = append(steps, &step.BlockId{ // uuid for device
		Device:      s.device,
		Format:      "value",
		MatchTag:    "UUID",
		Out:         &s.uuid,
		ExecOptions: curveadm.ExecOptions(),
	})
	steps = append(steps, &step.Lambda{
		Lambda: checkDeviceUUID(s.host, s.device, &success, &s.uuid),
	})
	steps = append(steps, &step.Lambda{ // generate record
		Lambda: s.expression(&express2del, &express2add),
	})
	if len(*s.oldUUID) > 0 {
		steps = append(steps, &step.Sed{ // remove old record
			Files:       []string{os.GetFSTabPath()},
			Expression:  &express2del,
			InPlace:     true,
			ExecOptions: curveadm.ExecOptions(),
		})
	}
	if !s.skipAdd {
		steps = append(steps, &step.Sed{ // add new record
			Files:       []string{os.GetFSTabPath()},
			Expression:  &express2add,
			InPlace:     true,
			ExecOptions: curveadm.ExecOptions(),
		})
	}

	for _, step := range steps {
		err := step.Execute(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *step2EditFSTab) Execute(ctx *context.Context) error {
	// lock by memstorage
	return s.curveadm.MemStorage().TX(func(m *utils.SafeMap) error {
		return s.execute(ctx)
	})
}

func device2ContainerName(device string) string {
	return fmt.Sprintf("curvebs-format-%s", utils.MD5Sum(device))
}

func NewFormatChunkfilePoolTask(curveadm *cli.CurveAdm, fc *configure.FormatConfig) (*task.Task, error) {
	host := fc.GetHost()
	hc, err := curveadm.GetHost(host)
	if err != nil {
		return nil, err
	}

	// new task
	device := fc.GetDevice()
	mountPoint := fc.GetMountPoint()
	usagePercent := fc.GetFormatPercent()
	subname := fmt.Sprintf("host=%s device=%s mountPoint=%s usage=%d%%",
		fc.GetHost(), device, mountPoint, usagePercent)
	t := task.NewTask("Start Format Chunkfile Pool", subname, hc.GetSSHConfig())

	// add step to task
	var oldContainerId, containerId, oldUUID string
	containerName := device2ContainerName(device)
	layout := topology.GetCurveBSProjectLayout()
	chunkfilePoolRootDir := layout.ChunkfilePoolRootDir
	formatScript := scripts.FORMAT
	formatScriptPath := fmt.Sprintf("%s/format.sh", layout.ToolsBinDir)
	formatCommand := fmt.Sprintf("%s %s %d %d %s %s", formatScriptPath, layout.FormatBinaryPath,
		usagePercent, DEFAULT_CHUNKFILE_SIZE, layout.ChunkfilePoolDir, layout.ChunkfilePoolMetaPath)

	// 1: skip if formating container exist
	t.AddStep(&step.ListContainers{
		ShowAll:     true,
		Format:      "'{{.ID}}'",
		Quiet:       true,
		Filter:      fmt.Sprintf("name=%s", containerName),
		Out:         &oldContainerId,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.Lambda{
		Lambda: skipFormat(&oldContainerId),
	})
	// 2: mkfs, mount device, edit fstab
	t.AddStep(&step.BlockId{
		Device:      device,
		Format:      "value",
		MatchTag:    "UUID",
		Out:         &oldUUID,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.UmountFilesystem{
		Directorys:     []string{device},
		IgnoreUmounted: true,
		IgnoreNotFound: true,
		ExecOptions:    curveadm.ExecOptions(),
	})
	t.AddStep(&step.CreateDirectory{
		Paths:       []string{mountPoint},
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.CreateFilesystem{ // mkfs.ext4 MOUNT_POINT
		Device:      device,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.MountFilesystem{
		Source:      device,
		Directory:   mountPoint,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step2EditFSTab{
		host:       host,
		device:     device,
		oldUUID:    &oldUUID,
		mountPoint: mountPoint,
		curveadm:   curveadm,
	})
	// 3: run container to format chunkfile pool
	t.AddStep(&step.PullImage{
		Image:       fc.GetContainerImage(),
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.CreateContainer{
		Image:       fc.GetContainerImage(),
		Command:     formatCommand,
		Entrypoint:  "/bin/bash",
		Name:        containerName,
		Remove:      true,
		Volumes:     []step.Volume{{HostPath: mountPoint, ContainerPath: chunkfilePoolRootDir}},
		Out:         &containerId,
		ExecOptions: curveadm.ExecOptions(),
	})
	t.AddStep(&step.InstallFile{
		ContainerId:       &containerId,
		ContainerDestPath: formatScriptPath,
		Content:           &formatScript,
		ExecOptions:       curveadm.ExecOptions(),
	})
	t.AddStep(&step.StartContainer{
		ContainerId: &containerId,
		ExecOptions: curveadm.ExecOptions(),
	})

	return t, nil
}
