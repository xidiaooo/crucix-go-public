import { appRoot } from '@prisma/composer';
import server from './service.mjs';

export default appRoot('crucix-go-public', ({ provision }) => {
  provision(server);
});
