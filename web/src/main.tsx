import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { ConfigProvider } from '@lobehub/ui';
import { motion } from 'motion/react';
import '@lobehub/ui/theme.css';
import '@lobehub/ui/style.css';
import './index.css';
import App from './App.tsx';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ConfigProvider motion={motion} locale="zh-CN">
      <App />
    </ConfigProvider>
  </StrictMode>,
);
