'use strict';

require('./launcher').post().catch(error => {
  console.error(`cluster cleanup: ${error.message}`);
  process.exitCode = 1;
});
