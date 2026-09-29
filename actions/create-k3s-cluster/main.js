'use strict';

require('../create-ci-cluster/launcher').main('create-k3s').catch(error => {
  console.error(`k3s cluster action: ${error.message}`);
  process.exitCode = 1;
});
