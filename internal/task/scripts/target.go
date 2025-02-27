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
 * Created Date: 2022-02-08
 * Author: Jingli Chen (Wine93)
 */

package scripts

/*
 * Usage: target USER VOLUME CREATE SIZE
 * Example: target curve test true 10
 * See Also: https://linux.die.net/man/8/tgtadm
 */
var TARGET = `
#!/usr/bin/env bash

g_user=$1
g_volume=$2
g_create=$3
g_size=$4
g_tid=1
g_image=cbd:pool/${g_volume}_${g_user}_
g_image_md5=$(echo -n ${g_image} | md5sum | awk '{ print $1 }')
g_targetname=iqn.$(date +"%Y-%m").com.opencurve:curve.${g_image_md5}

mkdir -p /curvebs/nebd/data/lock
touch /etc/curve/curvetab

if [ $g_create == "true" ]; then
    output=$(curve_ops_tool create -userName=$g_user -fileName=$g_volume -fileLength=$g_size)
    if [ $? -ne 0 ]; then
        if [ "$output" != "CreateFile fail with errCode: 101" ]; then
            exit 1
        fi
    fi
fi

for ((i=1;;i++)); do
    tgtadm --lld iscsi --mode target --op show --tid $i 1>/dev/null 2>&1
    if [ $? -ne 0 ]; then
        g_tid=$i
        break
    fi
done

tgtadm --lld iscsi \
   --mode target \
   --op new \
   --tid ${g_tid} \
   --targetname ${g_targetname}

tgtadm --lld iscsi \
    --mode logicalunit \
    --op new \
    --tid ${g_tid} \
    --lun 1 \
    --bstype curve \
    --backing-store ${g_image}

tgtadm --lld iscsi \
    --mode logicalunit \
    --op update \
    --tid ${g_tid} \
    --lun 1 \
    --params vendor_id=NetEase,product_id=CurveVolume,product_rev=2.0

tgtadm --lld iscsi \
    --mode target \
    --op bind \
    --tid ${g_tid} \
    -I ALL
`
