package virgo

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"text/template"
	"time"

	"github.com/digitalocean/go-libvirt"
)

var metaDataFmt = `
instance-id: iid-%s;
#hostname: %s
#local-hostname: %s`

var userDataFmt = `#cloud-config
users:
  - name: %s
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    # this is the outcome of the command openssl passwd -1 -salt SaltSalt $PASSWORD
    passwd: %s

write_files: 
- path: /provision.sh
  content: |
    %s
    

- path: /remove_cloud_init.sh 
  content: |
    #!/usr/bin/env bash
    echo 'datasource_list: [ None ]' | sudo -s tee /etc/cloud/cloud.cfg.d/90_dpkg.cfg
    sudo apt-get purge -y cloud-init
    sudo rm -rf /etc/cloud/; sudo rm -rf /var/lib/cloud/


password: %s
chpasswd: { expire: False }
ssh_pwauth: True

# upgrade packages on startup
package_upgrade: true

#run 'apt-get upgrade' or yum equivalent on first boot
apt_upgrade: true

runcmd:
  - bash /provision.sh
  - bash /remove_cloud_init.sh
  - shutdown

power_state:
  mode: reboot
`

type ProvisionConf struct {
	CloudImgURL  string `json:"cloud_img_url"`
	CloudImgName string `json:"cloud_img_name"`
	User         string `json:"user"`
	Passwd       string `json:"passwd"`
	RootImgGB    int    `json:"root_img_gb"`
	Provision    string
}

type NetIf struct {
	Type           string `json:"type"`
	Bridge         string `json:"bridge"`
	MacAddr        string `json:"mac_addr"`
	UnixSocketPath string `json:"unix_socket_path"`
	Queues         int    `json:"queues"`
}

type GuestConf struct {
	Name             string  `json:"name"`
	RootImgPath      string  `json:"root_img_path"`
	ConfigIsoPath    string  `json:"config_iso_path"`
	MemoryMB         int     `json:"guest_memory_mb"`
	NumVcpus         int     `json:"guest_num_vcpus"`
	HugepageSupport  bool    `json:"guest_hugepage_support"`
	HugepageSize     int     `json:"guest_hugepage_size"`
	HugepageSizeUnit string  `json:"guest_hugepage_size_unit"`
	HugepageNodeSet  string  `json:"guest_hugepage_node_set"`
	NetIfs           []NetIf `json:"guest_net_ifs"`
}

func createMetaDataFile(path, guest string) error {
	s := fmt.Sprintf(metaDataFmt, guest, guest, guest)
	if err := ioutil.WriteFile(path, []byte(s), 0644); err != nil {
		return err
	}
	return nil
}

func createUserDataFile(path, user, passwd, script string) error {
	cmd := exec.Command("openssl", "passwd", "-1", "-salt", "SaltSalt", passwd)
	hash, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to executed %v: %v", cmd.Args, err)
	}

	re := regexp.MustCompile("\n")
	indentedScript := re.ReplaceAllString(script, "\n    ")
	s := fmt.Sprintf(userDataFmt, user, hash, indentedScript, passwd)

	if err := ioutil.WriteFile(path, []byte(s), 0644); err != nil {
		return err
	}
	return nil
}

func createConfigIsoImage(path, guest, user, passwd, prov string) error {
	var metaDataPath = "meta-data"
	var userDataPath = "user-data"

	if err := createMetaDataFile(metaDataPath, guest); err != nil {
		return fmt.Errorf("failed to create meta-data file for cloud-init: %v", err)
	}
	//defer os.Remove(metaDataPath)

	if err := createUserDataFile(userDataPath, user, passwd, prov); err != nil {
		return fmt.Errorf("failed to create user-data file for cloud-init: %v", err)
	}
	//defer os.Remove(userDataPath)

	cmd := exec.Command("genisoimage", "-output", path, "-volid", "cidata", "-joliet", "-rock", userDataPath, metaDataPath)
	_, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to generated config iso: %v", err)
	}
	return nil
}

func domXML(g *GuestConf) (string, error) {
	t, err := template.New("domtmpl").
		Funcs(template.FuncMap{"minusOne": minusOne}).
		Parse(domTmpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %v", err)
	}

	var xml bytes.Buffer
	if err := t.Execute(&xml, g); err != nil {
		return "", fmt.Errorf("failed to execute template: %v", err)
	}

	return xml.String(), nil
}

func minusOne(x int) int {
	return x - 1
}

var domTmpl = `
<domain type='kvm'>
	<name>{{.Name}}</name>
  	<!-- uuid>4a9b3f53-fa2a-47f3-a757-dd87720d9d1d</uuid -->
  	<memory unit='MiB'>{{.MemoryMB}}</memory>
  	<currentMemory unit='MiB'>{{.MemoryMB}}</currentMemory>

	<!-- hugepages -->
	{{if .HugepageSupport}}
	<memoryBacking>
	<hugepages>
		<page size='{{.HugepageSize}}' unit='{{.HugepageSizeUnit}}' nodeset='{{.HugepageNodeSet}}'/>
	</hugepages>
	</memoryBacking>
	{{end}}

	<vcpu placement='static'>{{.NumVcpus}}</vcpu>
	<!-- cputune><shares>4096</shares>
	<vcpupin vcpu='0' cpuset='4'/>
	<vcpupin vcpu='1' cpuset='5'/>
	<emulatorpin cpuset='4,5'/></cputune -->

	<os>
    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='hd'/>
  	</os>
  	<features>
    <acpi/>
    <apic/>
  	</features>

	<!-- cpu topo -->
	<cpu mode='host-model'>
    <model fallback='allow'/>
    <topology sockets='1' cores='{{.NumVcpus}}' threads='1'/>
	<numa>
	<cell id='0' cpus='0-{{.NumVcpus | minusOne}}' memory='{{.MemoryMB}}' unit='MiB' {{if .HugepageSupport}}memAccess='shared'{{end}}/>
    </numa>
	</cpu>

	<on_poweroff>destroy</on_poweroff>
  	<on_reboot>restart</on_reboot>
  	<on_crash>destroy</on_crash>

	<devices>
	<emulator>/usr/bin/qemu-system-x86_64</emulator>
    <!-- emulator>/usr/bin/kvm-spice</emulator -->

   	<disk type='file' device='disk'>
	<driver name='qemu' type='qcow2'/>
	<source file='{{.RootImgPath}}'/>
	<target dev='vda' bus='virtio'/>
	<address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
	</disk>

	<disk type='file' device='disk'>
	<driver name='qemu' type='raw'/>
	<source file='{{.ConfigIsoPath}}'/>
	<target dev='vdb' bus='virtio'/>
	<address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
	</disk>

	<!-- network interfaces -->
	{{range .NetIfs}} 
	{{if eq .Type "bridge"}}
	<interface type='{{.Type}}'>
	<source bridge='{{.Bridge}}' />
	<model type='virtio' />
	</interface>
	{{ else if eq .Type "vhostuser"}}
	<interface type='{{.Type}}'>
	<mac address='{{.MacAddr}}'/>
	<source type='unix' path='{{.UnixSocketPath}}' mode='client'/>
	<model type='virtio' />
	<driver queues='{{.Queues}}'>
	<host mrg_rxbuf='on'/>
	</driver>
	</interface> 
	{{end}}
	{{end}}

 	<serial type='pty'>
	<target port='0'/>
	</serial>
	<console type='pty'>
	<target type='serial' port='0'/>
    </console>
  	</devices>
</domain>
`

func downloadCloudImage(url string) error {
	_, err := exec.Command("wget", "--no-clobber", url).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to download cloud image from %s: %v", url, err)
	}
	return nil
}

func NewLibvirtConn() (*libvirt.Libvirt, error) {
	sockpath := "/var/run/libvirt/libvirt-sock"
	c, err := net.DialTimeout("unix", sockpath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to open libvirt socket %s: %v", sockpath, err)
	}

	rpcconn := libvirt.New(c)
	if err = rpcconn.Connect(); err != nil {
		return nil, fmt.Errorf("failed to open connection with libvirt daemon: %v", err)
	}

	return rpcconn, nil
}

type StoragePoolTarget struct {
	XMLName xml.Name `xml:"target"`
	Path    string   `xml:"path"`
}

type StoragePoolDesc struct {
	XMLName xml.Name          `xml:"pool"`
	Target  StoragePoolTarget `xml:"target"`
}

func GetStoragePoolDesc(rpcconn *libvirt.Libvirt, p libvirt.StoragePool) (*StoragePoolDesc, error) {
	xmldesc, err := rpcconn.StoragePoolGetXMLDesc(p, libvirt.StorageXMLFlags(0))
	if err != nil {
		return nil, fmt.Errorf("failed to get storage pool's %s XML: %v", p.Name, err)
	}

	sp := &StoragePoolDesc{}
	if err := xml.Unmarshal([]byte(xmldesc), sp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal storage pool's XML: %v", err)
	}

	return sp, nil
}

func GuestImagePaths(l *libvirt.Libvirt, poolName, guest string) (rootImgPath, configIsoPath string, e error) {
	pool, err := l.StoragePoolLookupByName(poolName)
	if err != nil {
		e = fmt.Errorf("failed to lookup storage pool %s: %v", poolName, err)
		return
	}

	pdesc, err := GetStoragePoolDesc(l, pool)
	if err != nil {
		e = fmt.Errorf("failed to get storage pool's %s description: %v", pool.Name, err)
		return
	}

	if pdesc.Target.Path == "" {
		e = fmt.Errorf("storage pool %s has empty target path", pool.Name)
		return
	}

	configIsoPath = filepath.Join(pdesc.Target.Path, ConfigIsoName(guest))
	rootImgPath = filepath.Join(pdesc.Target.Path, RootImgName(guest))

	return
}

func createVolumes(l *libvirt.Libvirt, guest string, c *ProvisionConf) (rootImgPath, configIsoPath string, e error) {
	baseu, err := url.Parse(c.CloudImgURL)
	if err != nil {
		e = err
		return
	}

	imgu, err := url.Parse(c.CloudImgName)
	if err != nil {
		e = err
		return
	}

	url := baseu.ResolveReference(imgu).String()
	if err := downloadCloudImage(url); err != nil {
		e = fmt.Errorf("failed to download %s: %v", url, err)
		return
	}

	rootImgPath, configIsoPath, err = GuestImagePaths(l, DefaultPool(), guest)
	if err != nil {
		e = fmt.Errorf("failed to compute guest image paths: %v", err)
		return
	}

	if err := createConfigIsoImage(ConfigIsoName(guest), guest, c.User, c.Passwd, c.Provision); err != nil {
		e = fmt.Errorf("failed to create configuration iso image %s: %v", ConfigIsoName, err)
		return
	}

	if err := copyFile(ConfigIsoName(guest), configIsoPath); err != nil {
		e = fmt.Errorf("failed to copy configuration iso under storage pool's directory: %v", err)
		return
	}

	if err := copyFile(c.CloudImgName, rootImgPath); err != nil {
		e = fmt.Errorf("failed to copy cloud image under storage pool's directory: %v", err)
		return
	}

	pool, err := l.StoragePoolLookupByName(DefaultPool())
	if err != nil {
		e = fmt.Errorf("failed to lookup storage pool %s: %v", DefaultPool(), err)
		return
	}

	if err := l.StoragePoolRefresh(pool, 0); err != nil {
		e = err
		return
	}

	vol, err := l.StorageVolLookupByName(pool, RootImgName(guest))
	if err != nil {
		e = fmt.Errorf("failed to lookup storage volume %s under pool %s: %v", RootImgName, pool.Name, err)
		return
	}

	if err := l.StorageVolResize(vol, uint64(1024*1024*1024*c.RootImgGB), 0); err != nil {
		e = fmt.Errorf("failed to resize volume: %v", err)
		return
	}

	return
}

func LaunchGuest(l *libvirt.Libvirt, g *GuestConf) error {
	if g.ConfigIsoPath == "" || g.RootImgPath == "" {
		return fmt.Errorf("empty root image path or config iso path")
	}

	Undefine(l, g.Name)

	//xmlStr := domXMLStr(guest, rootImgPath, configIsoPath, g)
	xmlStr, err := domXML(g)
	dom, err := l.DomainDefineXML(xmlStr)
	if err != nil {
		return fmt.Errorf("failed to define domain %s from xml: %v", g.Name, err)
	}

	if err := l.DomainCreate(dom); err != nil {
		return fmt.Errorf("failed to create domain %s: %v", dom.Name, err)
	}

	return nil
}

func DefaultPool() string {
	return "default"
}

func RootImgName(guest string) string {
	return fmt.Sprintf("%s.virgo.img", guest)
}

func ConfigIsoName(guest string) string {
	return fmt.Sprintf("%s.virgo.iso", guest)
}

func Provision(l *libvirt.Libvirt, p *ProvisionConf, g *GuestConf) error {
	var err error
	g.RootImgPath, g.ConfigIsoPath, err = createVolumes(l, g.Name, p)
	if err != nil {
		return fmt.Errorf("failed to create volumes: %v", err)
	}

	if err := LaunchGuest(l, g); err != nil {
		return fmt.Errorf("failed to create guest: %v", err)
	}

	return nil
}

func Start(l *libvirt.Libvirt, guest string) error {
	dom, err := l.DomainLookupByName(guest)
	if err != nil {
		return fmt.Errorf("failed to lookup domain %s: %v", guest, err)
	}

	if err := l.DomainCreate(dom); err != nil {
		return fmt.Errorf("failed to create domain %s: %v", guest, err)
	}

	return nil
}

func Stop(l *libvirt.Libvirt, guest string) error {
	dom, err := l.DomainLookupByName(guest)
	if err != nil {
		return fmt.Errorf("failed to lookup domain %s: %v", guest, err)
	}

	if err := l.DomainShutdown(dom); err != nil {
		return fmt.Errorf("failed to shutdown domain %s: %v", guest, err)
	}

	return nil
}

func Undefine(l *libvirt.Libvirt, guest string) error {
	dom, err := l.DomainLookupByName(guest)
	if err != nil {
		return fmt.Errorf("failed to lookup domain %s: %v", guest, err)
	}

	l.DomainShutdown(dom)

	if err := l.DomainUndefine(dom); err != nil {
		return fmt.Errorf("failed to undefine domain %s: %v", dom.Name, err)
	}

	return nil
}

func Purge(l *libvirt.Libvirt, guest string) error {
	pool, err := l.StoragePoolLookupByName(DefaultPool())
	if err != nil {
		return fmt.Errorf("failed to lookup storage pool %s: %v", DefaultPool(), err)
	}

	rootVol, err := l.StorageVolLookupByName(pool, RootImgName(guest))
	if err != nil {
		return fmt.Errorf("failed to lookup storage volume %s under pool %s: %v", RootImgName(guest), pool.Name, err)
	}

	if err := l.StorageVolDelete(rootVol, 0); err != nil {
		return fmt.Errorf("failed to delete storage volume %s: %v", rootVol.Name, err)
	}

	configVol, err := l.StorageVolLookupByName(pool, ConfigIsoName(guest))
	if err != nil {
		return fmt.Errorf("failed to lookup storage volume %s under pool %s: %v", ConfigIsoName(guest), pool.Name, err)
	}

	if err := l.StorageVolDelete(configVol, 0); err != nil {
		return fmt.Errorf("failed to delete storage volume %s: %v", configVol.Name, err)
	}

	if err := l.StoragePoolRefresh(pool, 0); err != nil {
		return err
	}

	return nil
}

func copyFile(srcPath, dstPath string) error {
	in, err := ioutil.ReadFile(srcPath)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(dstPath, in, 0644); err != nil {
		return err
	}
	return nil
}
