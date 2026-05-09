# onvif-bridge — pilot deployment

Deploys go2rtc with the per-profile `device_info` patch as a virtual ONVIF
device on the lab VM, fronting one upstream RTSP camera. Designed for Nx
Witness to discover-by-IP and allocate an Encoder license rather than a
generic Pro license.

## Deployment shape

- **Target host**: `cel-lan-lab-01` (10.22.40.3, VMS-MGMT VLAN 40)
- **Networking**: host networking — virtual ONVIF device exposed at
  `10.22.40.3:8081`. No macvlan, no extra IPs.
- **Build**: Portainer instructs the lab VM's Docker daemon to clone this
  fork on the `feat/configurable-deviceinfo` branch and build the image
  locally. No registry needed.
- **Lab pattern**: stack lives only in Portainer + this fork; nothing in
  the IaC repo (lab is firebreaked from production by design).

## Deploy via Portainer

1. Stacks → Add stack → Repository
2. Repository URL: `https://github.com/jc-5674/go2rtc`
3. Repository reference: `feat/configurable-deviceinfo`
4. Compose path: `deploy/onvif-bridge/docker-compose.yml`
5. Edit the `go2rtc.yaml` (Portainer Web Editor → file tree on the right)
   and replace the 5 `<FILL ME>` placeholders with the pilot camera's
   actual RTSP URLs + the spoofed encoder identity strings.
6. Deploy.

## Validation

```sh
# 1. Container up
docker logs onvif-bridge 2>&1 | grep -E "ONVIF|listen"

# 2. ONVIF device responds
curl -s -X POST http://10.22.40.3:8081/onvif/device_service \
  -H 'Content-Type: application/soap+xml' \
  --data '<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><s:Body><tds:GetDeviceInformation/></s:Body></s:Envelope>'
# Expect: Manufacturer/Model/SerialNumber/HardwareId returning the
# spoofed encoder identity from go2rtc.yaml.
```

3. In Nx Witness Server: Add Camera → Generic ONVIF → `10.22.40.3:8081`,
   credentials `admin`/anything. Confirm Nx shows the spoofed
   Manufacturer + Model and allocates an **Encoder** license slot
   (not Professional).

4. Tail `docker logs -f onvif-bridge` while previewing in Nx — confirm
   one upstream RTSP session is opened to the camera regardless of how
   many Nx clients view simultaneously (this is the fan-out win).

5. Leave running 24–48h; observe whether Dahua-era stream drops are gone.

## Adding more cameras

Edit `go2rtc.yaml`:

```yaml
streams:
  cam01: [ ... ]
  cam02:
    - "rtsp://user:pass@192.168.x.y2:554/.../subtype=0"
    - "rtsp://user:pass@192.168.x.y2:554/.../subtype=1"

onvif:
  profiles:
    - name: cam01 ...
    - name: cam02
      port: 8082
      streams:
        - "cam02#res=...#..."
        - "cam02#res=...#..."
      device_info:
        manufacturer: "Dahua"
        model: "<encoder model>"
        firmware_version: "..."
        serial_number: "PILOT-CAM-02"     # MUST be unique per virtual device
        hardware_id: "1.00"
```

Restart the stack in Portainer. Add the new device to Nx at
`10.22.40.3:<new port>`.

## Graduating to production

If/when this proves itself across the fleet:
1. PR `feat/configurable-deviceinfo` upstream to NickWaterton/go2rtc.
2. Move deployment to its own VM (e.g. `cel-lan-onvif-01` on VMS-MGMT)
   with a proper Ansible role under `roles/onvif_bridge` in the IaC repo.
3. Stack file (in `stacks/onvif-bridge/`) deployed via the production
   playbook rather than Portainer Repository.

The lab → production handover is when this stops being firebreaked.
