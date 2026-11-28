import { compute } from '@prisma/composer-prisma-cloud';
import nodeBuild from '@prisma/composer/node';

export default compute({
  name: 'server',
  deps: {},
  build: nodeBuild({
    module: import.meta.url,
    dir: './dist',
    entry: './server.mjs',
  }),
});
