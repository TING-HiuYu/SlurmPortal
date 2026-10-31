import { Flex } from "antd";

// Page title row: title (and optional description) on the left, actions on the right.
export default function PageHead({ title, desc, children }) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {desc && <p>{desc}</p>}
      </div>
      {children && <Flex gap={10} wrap align="center">{children}</Flex>}
    </div>
  );
}
