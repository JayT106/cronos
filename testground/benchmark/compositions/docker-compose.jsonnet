std.manifestYamlDoc({
  services: {
    ['testplan-' + i]: {
      image: 'cronos-testground:latest',
      command: 'stateless-testcase run',
      container_name: 'testplan-' + i,
      volumes: [
        std.extVar('outputs') + ':/outputs',
      ],
      environment: {
        JOB_COMPLETION_INDEX: i,
      },
      sysctls: {
        'net.core.rmem_max': 8441037,
        'net.core.wmem_max': 8441037,
      },
    }
    for i in std.range(0, std.extVar('nodes') - 1)
  },
})
