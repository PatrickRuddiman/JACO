// testbed/template.bicep — deploys N identical Debian VMs (<prefix>-1,
// <prefix>-2, ..., <prefix>-N) into a shared VNet + subnet + NSG, for the
// comparative orchestrator benchmark in ../samples.
//
// Network model (Tailscale was removed in v0.1.0):
//   * Each VM gets its OWN Standard public IP on the NIC. SSH (22) is reachable
//     directly per node — no overlay/VPN. This is what makes the bed work as a
//     neutral substrate for ANY orchestrator (JACO, k8s, k3s, swarm), none of
//     which should assume a mesh exists at provisioning time.
//   * A public Standard Load Balancer additionally fronts 80 + 443 across all
//     VMs (persistent lb-pip in a separate RG) so whichever stack is deployed
//     serves north-south ingress at one stable IP for the jaco.sh A record.
//
// customData is rendered from cloud-init.yaml.tpl by deploy.sh and passed in
// base64 — it is intentionally MINIMAL (base OS only, no container runtime, no
// orchestrator) so every stack's bootstrap starts from an identical baseline.

targetScope = 'resourceGroup'

@description('Azure region for all resources.')
param location string = 'centralus'

@description('Number of VMs to deploy. VM names: <prefix>-1, <prefix>-2, ...')
param vmCount int = 3

@description('Name prefix for VMs, NICs, and per-VM public IPs.')
param vmNamePrefix string = 'jaco'

@description('VNet name. Created if missing.')
param virtualNetworkName string = 'vnet-centralus-1'

@description('Subnet name within the VNet.')
param subnetName string = 'snet-centralus-1'

@description('VNet address space.')
param vnetAddressPrefix string = '172.16.0.0/16'

@description('Subnet CIDR.')
param subnetAddressPrefix string = '172.16.0.0/24'

@description('Resource group holding the persistent LB public IP. Kept separate from the main RG so teardown.sh never deletes the IP — the jaco.sh A record survives rebuilds. deploy.sh creates the RG + IP idempotently.')
param publicIpResourceGroup string = 'jaco-net'

@description('Name of the persistent LB public IP resource (in publicIpResourceGroup).')
param publicIpName string = 'jaco-lb-pip'

@description('VM size.')
param virtualMachineSize string = 'Standard_B2s'

@description('OS disk storage tier.')
param osDiskType string = 'StandardSSD_LRS'

@description('OS disk size in GiB.')
param osDiskSizeGiB int = 128

@description('Admin username for SSH access (over the per-VM public IP).')
param adminUsername string = 'azureuser'

@description('SSH public key (single line, OpenSSH format). Public keys are not secret, but deploy.sh sources it from the environment so the bed is reproducible without editing this file.')
@secure()
param adminPublicKey string

@description('Source address prefix allowed to reach SSH (22). Defaults to Internet for an open bed; set to your operator IP/CIDR to lock it down.')
param sshSourceAddressPrefix string = 'Internet'

@description('Daily auto-shutdown time in HHmm (24h, no colon).')
param autoShutdownTime string = '0200'

@description('IANA-style timezone for the auto-shutdown schedule.')
param autoShutdownTimeZone string = 'Eastern Standard Time'

@description('Optional email for auto-shutdown notifications. Empty disables them.')
param autoShutdownNotificationEmail string = ''

@allowed([ 'Enabled', 'Disabled' ])
@description('Enable or disable the daily auto-shutdown schedule.')
param autoShutdownStatus string = 'Enabled'

@description('Base64-encoded cloud-init payload. Rendered + base64\'d by deploy.sh from cloud-init.yaml.tpl. Marked secure so it never appears in deployment logs. Default \'\' lets a bare `bicep build` lint without env.')
@secure()
param customData string = ''

resource virtualNetwork 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: virtualNetworkName
  location: location
  properties: {
    addressSpace: {
      addressPrefixes: [
        vnetAddressPrefix
      ]
    }
    subnets: [
      {
        name: subnetName
        properties: {
          addressPrefix: subnetAddressPrefix
        }
      }
    ]
  }
}

// NSG: SSH (22) is open per the bed's neutral-substrate role, plus 80/443 so
// the public Standard LB can reach whichever orchestrator's ingress is running
// on each VM. Lock SSH down by setting sshSourceAddressPrefix to your CIDR.
resource networkSecurityGroup 'Microsoft.Network/networkSecurityGroups@2023-09-01' = {
  name: '${vmNamePrefix}-nsg'
  location: location
  properties: {
    securityRules: [
      {
        name: 'AllowSshInbound'
        properties: {
          priority: 100
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          destinationPortRange: '22'
          sourceAddressPrefix: sshSourceAddressPrefix
          destinationAddressPrefix: '*'
        }
      }
      {
        name: 'AllowHttpInbound'
        properties: {
          priority: 110
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          destinationPortRange: '80'
          sourceAddressPrefix: 'Internet'
          destinationAddressPrefix: '*'
        }
      }
      {
        name: 'AllowHttpsInbound'
        properties: {
          priority: 120
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          destinationPortRange: '443'
          sourceAddressPrefix: 'Internet'
          destinationAddressPrefix: '*'
        }
      }
    ]
  }
}

// Persistent LB public IP, referenced as `existing` from a separate RG so
// teardown (which deletes only the main RG) never destroys it. The jaco.sh A
// record stays valid across rebuilds. deploy.sh creates it idempotently.
resource publicIp 'Microsoft.Network/publicIPAddresses@2024-01-01' existing = {
  name: publicIpName
  scope: resourceGroup(publicIpResourceGroup)
}

// One ephemeral Standard public IP per VM, for direct SSH + per-node egress.
// Created and deleted with the main RG (so teardown reclaims them); only the
// LB frontend IP above is persistent.
resource vmPublicIp 'Microsoft.Network/publicIPAddresses@2024-01-01' = [for i in range(0, vmCount): {
  name: '${vmNamePrefix}-${i + 1}-pip'
  location: location
  sku: {
    name: 'Standard'
  }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}]

// Standard SKU Load Balancer: one frontend IP, one backend pool with all VMs,
// TCP/80 health probe, LB rules for 80 + 443. No outbound rule — each VM's
// instance-level public IP provides its own outbound SNAT, so the LB serves
// inbound ingress only (disableOutboundSnat:true on the rules).
resource loadBalancer 'Microsoft.Network/loadBalancers@2024-01-01' = {
  name: '${vmNamePrefix}-lb'
  location: location
  sku: {
    name: 'Standard'
    tier: 'Regional'
  }
  properties: {
    frontendIPConfigurations: [
      {
        name: 'lbFrontend'
        properties: {
          publicIPAddress: {
            id: publicIp.id
          }
        }
      }
    ]
    backendAddressPools: [
      {
        name: 'lbBackendPool'
      }
    ]
    probes: [
      {
        name: 'http-probe'
        properties: {
          protocol: 'Tcp'
          port: 80
          intervalInSeconds: 5
          numberOfProbes: 2
        }
      }
    ]
    loadBalancingRules: [
      {
        name: 'http-rule'
        properties: {
          frontendIPConfiguration: {
            id: resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', '${vmNamePrefix}-lb', 'lbFrontend')
          }
          backendAddressPool: {
            id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '${vmNamePrefix}-lb', 'lbBackendPool')
          }
          probe: {
            id: resourceId('Microsoft.Network/loadBalancers/probes', '${vmNamePrefix}-lb', 'http-probe')
          }
          protocol: 'Tcp'
          frontendPort: 80
          backendPort: 80
          enableFloatingIP: false
          idleTimeoutInMinutes: 4
          loadDistribution: 'Default'
          disableOutboundSnat: true
        }
      }
      {
        name: 'https-rule'
        properties: {
          frontendIPConfiguration: {
            id: resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', '${vmNamePrefix}-lb', 'lbFrontend')
          }
          backendAddressPool: {
            id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '${vmNamePrefix}-lb', 'lbBackendPool')
          }
          probe: {
            id: resourceId('Microsoft.Network/loadBalancers/probes', '${vmNamePrefix}-lb', 'http-probe')
          }
          protocol: 'Tcp'
          frontendPort: 443
          backendPort: 443
          enableFloatingIP: false
          idleTimeoutInMinutes: 4
          loadDistribution: 'Default'
          disableOutboundSnat: true
        }
      }
    ]
  }
}

resource networkInterface 'Microsoft.Network/networkInterfaces@2023-09-01' = [for i in range(0, vmCount): {
  name: '${vmNamePrefix}-${i + 1}-nic'
  location: location
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          subnet: {
            id: '${virtualNetwork.id}/subnets/${subnetName}'
          }
          privateIPAllocationMethod: 'Dynamic'
          primary: true
          publicIPAddress: {
            id: vmPublicIp[i].id
          }
          loadBalancerBackendAddressPools: [
            {
              id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '${vmNamePrefix}-lb', 'lbBackendPool')
            }
          ]
        }
      }
    ]
    networkSecurityGroup: {
      id: networkSecurityGroup.id
    }
  }
  dependsOn: [
    loadBalancer
  ]
}]

resource virtualMachine 'Microsoft.Compute/virtualMachines@2024-03-01' = [for i in range(0, vmCount): {
  name: '${vmNamePrefix}-${i + 1}'
  location: location
  properties: {
    hardwareProfile: {
      vmSize: virtualMachineSize
    }
    storageProfile: {
      osDisk: {
        createOption: 'FromImage'
        managedDisk: {
          storageAccountType: osDiskType
        }
        diskSizeGB: osDiskSizeGiB
        deleteOption: 'Delete'
      }
      imageReference: {
        publisher: 'debian'
        offer: 'debian-13'
        sku: '13-gen2'
        version: 'latest'
      }
    }
    networkProfile: {
      networkInterfaces: [
        {
          id: networkInterface[i].id
          properties: {
            deleteOption: 'Delete'
          }
        }
      ]
    }
    osProfile: {
      computerName: '${vmNamePrefix}-${i + 1}'
      adminUsername: adminUsername
      customData: customData
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/${adminUsername}/.ssh/authorized_keys'
              keyData: adminPublicKey
            }
          ]
        }
        patchSettings: {
          assessmentMode: 'ImageDefault'
          patchMode: 'ImageDefault'
        }
      }
    }
  }
}]

resource shutdownSchedule 'Microsoft.DevTestLab/schedules@2018-09-15' = [for i in range(0, vmCount): {
  name: 'shutdown-computevm-${vmNamePrefix}-${i + 1}'
  location: location
  properties: {
    status: autoShutdownStatus
    taskType: 'ComputeVmShutdownTask'
    dailyRecurrence: {
      time: autoShutdownTime
    }
    timeZoneId: autoShutdownTimeZone
    targetResourceId: virtualMachine[i].id
    notificationSettings: {
      status: empty(autoShutdownNotificationEmail) ? 'Disabled' : 'Enabled'
      notificationLocale: 'en'
      timeInMinutes: 30
      emailRecipient: autoShutdownNotificationEmail
    }
  }
}]

output vmNames array = [for i in range(0, vmCount): '${vmNamePrefix}-${i + 1}']
output vmPublicIps array = [for i in range(0, vmCount): vmPublicIp[i].properties.ipAddress]
output loadBalancerPublicIp string = publicIp.properties.ipAddress
output loadBalancerName string = '${vmNamePrefix}-lb'
