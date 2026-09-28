'use strict';

require('./launcher').main().catch(error => {
  console.error(`cluster action: ${error.message}`);
  process.exitCode = 1;
});
