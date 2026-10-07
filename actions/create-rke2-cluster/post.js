'use strict';

require('../create-ci-cluster/launcher').post().catch(error => {
  console.error(`RKE2 cluster cleanup: ${error.message}`);
  process.exitCode = 1;
});
