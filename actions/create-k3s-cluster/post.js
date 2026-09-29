'use strict';

require('../create-ci-cluster/launcher').post().catch(error => {
  console.error(`k3s cluster cleanup: ${error.message}`);
  process.exitCode = 1;
});
