'use strict';

require('../create-ci-cluster/launcher').main('create-rke2').catch(error => {
  console.error(`RKE2 cluster action: ${error.message}`);
  process.exitCode = 1;
});
